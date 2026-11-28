/* Scope Audit: one bulk request, local search, and a bounded keyed row diff.
 * Two views share the controls: declarations (#378) and observed transport
 * (#414). Their evidence is kept apart; the transport view never turns
 * absence into a configuration verdict. */
(function(root) {
  'use strict';
  var PAGE_SIZE = 50;
  function windowValue(value) { return ['1h', '24h', '7d'].indexOf(value) >= 0 ? value : '24h'; }
  // Firmware matches region names exactly (strcmp); only a leading # is optional.
  function normalizeScope(value) { return String(value || '').trim().replace(/^#/, '').trim(); }
  function stateFromHash(hash) {
    var p = new URLSearchParams(String(hash || '').split('?')[1] || '');
    return { window: windowValue(p.get('swin')), query: p.get('saq') || '', page: Math.max(0, Math.floor(Number(p.get('sap')) || 0)),
      view: p.get('sav') === 'transport' ? 'transport' : 'declared', scope: normalizeScope(p.get('scope')) };
  }
  function transportPath(state, region) {
    return '/scope-audit/transport?window=' + encodeURIComponent(windowValue(state.window)) +
      (state.scope ? '&scope=' + encodeURIComponent(state.scope) : '') + (region ? '&region=' + encodeURIComponent(region) : '');
  }
  function searchRows(rows, query) {
    var q = String(query || '').trim().toLowerCase();
    return rows.filter(function(r) {
      return !q || [r.name, r.publicKey, r.configuredScope].concat(r.declaredScopes || []).join(' ').toLowerCase().indexOf(q) >= 0;
    });
  }
  function searchTransportRows(rows, query) {
    var q = String(query || '').trim().toLowerCase();
    return rows.filter(function(r) {
      if (!q) return true;
      var d = r.declaration || {};
      return [r.name, r.publicKey].concat(d.declaredScopes || [], (r.observedScopes || []).map(function(s) { return s.name; })).join(' ').toLowerCase().indexOf(q) >= 0;
    });
  }
  function declarationAge(seconds) {
    if (seconds == null || !Number.isFinite(Number(seconds))) return 'Unknown';
    var n = Math.max(0, Number(seconds));
    return n < 3600 ? Math.floor(n / 60) + 'm' : n < 86400 ? Math.floor(n / 3600) + 'h' : Math.floor(n / 86400) + 'd';
  }
  function count(value) { return Math.max(0, Number(value) || 0); }
  function assessment(row) {
    if (row.status === 'no-evidence' || !count(row.forwarded)) return 'No forwarding evidence';
    var findings = (row.undeclaredObserved || []).length || row.wildcardContradiction;
    if (row.incompleteEvidence || row.staleDeclaration || count(row.unknownScopeObserved)) return findings ? 'Findings · incomplete evidence' : 'Incomplete evidence';
    return findings || (row.notObserved || []).length ? 'Findings to investigate' : 'Consistent with observations';
  }
  // Labels for the server's transport evidence categories (scope_audit_transport.go).
  function transportEvidence(row, scope, minForwarded) {
    var s = String(scope || '');
    switch (row.evidence) {
      case 'carrier': return { title: 'Seen carrying ' + s, detail: '' };
      case 'not-seen-carrying': return { title: 'Not seen carrying ' + s, detail: 'Observation only, not a configuration finding', caution: true };
      case 'ambiguous': return { title: 'Ambiguous attribution', detail: 'A hop prefix it shares with another repeater carried ' + s };
      case 'incomplete': return { title: 'Incomplete evidence', detail: 'Unknown scopes or frames stored without headers may hide ' + s };
      case 'low-evidence': return { title: 'Too little traffic', detail: 'Fewer than ' + count(minForwarded) + ' forwarded transmissions' };
      case 'no-scope-traffic': return { title: 'No ' + s + ' traffic here', detail: 'Nobody in this population was seen carrying it' };
      default: return { title: 'Forwarding observed', detail: s ? '' : 'Choose a scope to compare carriers' };
    }
  }
  var VIEWS = {
    declared: {
      columns: ['Repeater', 'Declaration / age', 'Declared scopes', 'Observed forwarding', 'Assessment'],
      intro: ['Whole-network audit. The audit window is independent of the global time and region filters.',
        'Compares confirmed observer declarations with forwarding observed in the selected window. A declared scope without observed traffic does not prove a configuration fault. Observer coverage, quiet regions and configuration changes can explain gaps.',
        'Short windows may contain little traffic; 7d may span configuration changes. Declarations older than 7d or with an unknown timestamp, unknown scopes and ambiguous hop attribution make the evidence incomplete.']
    },
    transport: {
      columns: ['Repeater', 'Declaration (separate)', 'Seen carrying', 'Carrier / non-carrier', 'Transport evidence'],
      intro: ['Every repeater seen forwarding flood traffic in the selected window, including repeaters never asked for a declaration. Choose a scope to see who was seen carrying it. The global region filter limits the population to observers in those regions; counts, rows and details all use it.',
        'Limitations: transport evidence is what observers heard forwarded, not what a repeater is configured to do. Not seen carrying a scope is never a configuration fault: quiet traffic, observer coverage and the paths that happened to be heard explain absence.',
        'Only TRANSPORT_FLOOD and FLOOD count; each transmission counts once per repeater. A hop prefix shared by several repeaters is credited to none of them, so repeaters seen only through shared prefixes are not listed. Unknown scopes, frames stored without headers and too little traffic are reported as their own categories. Results are cached for 30 seconds.']
    }
  };
  function mount(container, options) {
    var doc = container.ownerDocument;
    var rows = [], rowNodes = new Map(), generation = 0, active = true, searchTimer = null, minForwarded = 0, responseScope = '';
    var state = stateFromHash(options.hash());
    var regionParam = options.region || function() { return ''; };
    function element(tag, text, cls) {
      var el = doc.createElement(tag);
      if (text != null) el.textContent = String(text);
      if (cls) el.className = cls;
      return el;
    }
    function add(parent, tag, text, cls) { var el = element(tag, text, cls); parent.appendChild(el); return el; }
    add(container, 'h3', 'Scope Audit');
    var switcher = add(container, 'div', null, 'scope-audit-controls');
    var viewButtons = ['declared', 'transport'].map(function(v) {
      var btn = add(switcher, 'button', v === 'declared' ? 'Declarations' : 'Transport', 'tab-btn'); btn.dataset.auditView = v;
      btn.addEventListener('click', function() {
        if (state.view === v) return;
        state.view = v; state.page = 0; save(); return load(state.window);
      });
      return btn;
    });
    var intro = add(container, 'div'); intro.id = 'scope-audit-intro';
    var toolbar = add(container, 'div', null, 'scope-audit-controls');
    ['1h', '24h', '7d'].forEach(function(w) {
      var btn = add(toolbar, 'button', w, 'tab-btn'); btn.dataset.win = w; btn.dataset.auditWindow = 'true';
      btn.addEventListener('click', function() { options.onWindow(w); });
    });
    var scopeLabel = add(toolbar, 'label', 'Scope '); scopeLabel.id = 'scope-audit-scope-label';
    var scopeInput = add(scopeLabel, 'input'); scopeInput.type = 'search'; scopeInput.id = 'scope-audit-scope'; scopeInput.value = state.scope;
    scopeInput.placeholder = 'e.g. dk-nord'; scopeInput.setAttribute('list', 'scope-audit-scope-list');
    var scopeList = add(toolbar, 'datalist'); scopeList.id = 'scope-audit-scope-list';
    scopeInput.addEventListener('change', function() {
      if (!active || !container.isConnected) return;
      var next = normalizeScope(scopeInput.value);
      scopeInput.value = next;
      if (next === state.scope) return;
      state.scope = next; state.page = 0; save(); return load(state.window);
    });
    var label = add(toolbar, 'label', 'Search repeaters / scopes ');
    var input = add(label, 'input'); input.type = 'search'; input.id = 'scope-audit-search'; input.value = state.query;
    input.placeholder = 'Name, public key or scope';
    var refresh = add(toolbar, 'button', 'Refresh', 'tab-btn');
    refresh.addEventListener('click', function() { load(state.window, true); });
    var message = add(container, 'p', '', 'text-muted'); message.id = 'scope-audit-message'; message.setAttribute('role', 'status');
    var summary = add(container, 'p'); summary.id = 'scope-audit-summary';
    var population = add(container, 'p', '', 'text-muted'); population.id = 'scope-audit-population';
    var wrap = add(container, 'div', null, 'scope-audit-table-wrap');
    var table = add(wrap, 'table', null, 'data-table scope-audit-table'); table.id = 'scope-audit-table';
    var header = add(add(table, 'thead'), 'tr');
    var tbody = add(table, 'tbody'); tbody.id = 'scope-audit-rows';
    var pager = add(container, 'div', null, 'scope-audit-controls');
    var previous = add(pager, 'button', 'Previous', 'tab-btn');
    var pageLabel = add(pager, 'span');
    var next = add(pager, 'button', 'Next', 'tab-btn');
    var shownView = null;
    // Header, intro and the row cache belong to one view; switching rebuilds them.
    function applyView() {
      if (shownView === state.view) return;
      shownView = state.view;
      var view = VIEWS[state.view];
      intro.textContent = '';
      view.intro.forEach(function(t) { add(intro, 'p', t, 'text-muted'); });
      header.textContent = '';
      view.columns.forEach(function(t) { add(header, 'th', t); });
      table.className = 'data-table scope-audit-table' + (state.view === 'transport' ? ' scope-transport-table' : '');
      rowNodes.forEach(function(cached) { cached.el.remove(); }); rowNodes.clear(); rows = [];
      summary.textContent = ''; population.textContent = '';
      scopeLabel.hidden = state.view !== 'transport';
      container.classList.toggle('scope-audit-transport', state.view === 'transport');
      viewButtons.forEach(function(b) { b.classList.toggle('active', b.dataset.auditView === state.view); });
    }
    function save() { options.onState(state.query, state.page, state.view, state.scope); }
    previous.addEventListener('click', function() { state.page = Math.max(0, state.page - 1); save(); render(); });
    next.addEventListener('click', function() { state.page++; save(); render(); });
    input.addEventListener('input', function() {
      if (!active || !container.isConnected) return;
      clearTimeout(searchTimer);
      state.query = input.value; state.page = 0; save();
      searchTimer = setTimeout(function() { if (active && container.isConnected) render(); }, 150);
    });
    function cell(tr, labelText) { var td = add(tr, 'td'); td.dataset.label = labelText; return td; }
    function nameCell(tr, r) {
      var name = cell(tr, 'Repeater');
      var link = add(name, 'a', r.name || String(r.publicKey || '').slice(0, 16));
      link.href = '#/nodes/' + encodeURIComponent(String(r.publicKey || ''));
      add(name, 'div', r.role || 'Repeater', 'text-muted');
    }
    function rowElement(r) {
      var tr = element('tr'); tr.dataset.publicKey = String(r.publicKey || '');
      nameCell(tr, r);
      var declaration = add(tr, 'td');
      add(declaration, 'div', r.configuredScope === '' ? 'Empty declaration · no flood scopes' : r.allowsUnscoped ? 'Allows unscoped (*)' : 'Scoped only');
      add(declaration, 'div', declarationAge(r.declarationAgeSeconds) + (r.staleDeclaration ? ' · stale / unknown timestamp' : ''), 'text-muted');
      if (r.configuredScopeAt) declaration.title = String(r.configuredScopeAt);
      var scopes = add(tr, 'td');
      var observed = new Set((r.observedScopes || []).map(function(s) { return String(s.name); }));
      (r.declaredScopes || []).forEach(function(s) {
        add(scopes, 'span', String(s) + (observed.has(String(s)) ? ' · observed' : ' · not observed'), 'scope-audit-chip' + (observed.has(String(s)) ? '' : ' scope-audit-caution'));
      });
      if (!(r.declaredScopes || []).length) add(scopes, 'span', 'No named scopes', 'text-muted');
      var traffic = add(tr, 'td');
      (r.observedScopes || []).forEach(function(s) { add(traffic, 'div', String(s.name) + ': ' + count(s.count)); });
      add(traffic, 'div', 'Unscoped: ' + count(r.unscopedObserved));
      if (count(r.unknownScopeObserved)) add(traffic, 'div', 'Unknown scopes: ' + count(r.unknownScopeObserved), 'scope-audit-caution');
      if (count(r.ambiguousHops)) add(traffic, 'div', 'Ambiguous hops: ' + count(r.ambiguousHops), 'scope-audit-caution');
      var status = add(tr, 'td');
      add(status, 'strong', assessment(r));
      if ((r.notObserved || []).length) add(status, 'div', count(r.notObserved.length) + ' declared scope(s) not observed', 'text-muted');
      if ((r.undeclaredObserved || []).length) add(status, 'div', 'Observed outside declaration: ' + r.undeclaredObserved.join(', '), 'scope-audit-caution');
      if (r.wildcardContradiction) add(status, 'div', 'Unscoped forwarding without declared *', 'scope-audit-caution');
      return tr;
    }
    function transportRowElement(r) {
      var tr = element('tr'); tr.dataset.publicKey = String(r.publicKey || '');
      var scope = responseScope;
      nameCell(tr, r);
      var declaration = cell(tr, 'Declaration'), d = r.declaration;
      if (!d) {
        add(declaration, 'div', 'Never asked');
        add(declaration, 'div', 'No declaration answer: not the same as an empty declaration', 'text-muted');
      } else {
        add(declaration, 'div', d.configuredScope === '' ? 'Empty declaration' : (d.declaredScopes || []).join(', ') + (d.allowsUnscoped ? (d.declaredScopes || []).length ? ', *' : '*' : ''));
        if (scope && d.declaresScope != null) add(declaration, 'div', (d.declaresScope ? 'Declares ' : 'Does not declare ') + scope, 'text-muted');
        if (d.staleDeclaration) add(declaration, 'div', 'Stale / unknown timestamp', 'text-muted');
        if (d.configuredScopeAt) declaration.title = String(d.configuredScopeAt);
      }
      var carried = cell(tr, 'Seen carrying');
      (r.observedScopes || []).forEach(function(s) {
        add(carried, 'span', String(s.name) + ': ' + count(s.count), 'scope-audit-chip' + (String(s.name) === scope ? ' scope-audit-selected' : ''));
      });
      if (r.observedScopesTruncated) add(carried, 'span', 'more…', 'text-muted');
      if (count(r.unscoped)) add(carried, 'div', 'Unscoped: ' + count(r.unscoped));
      if (count(r.unknownScope)) add(carried, 'div', 'Unknown scopes: ' + count(r.unknownScope), 'scope-audit-caution');
      if (count(r.ambiguousHops)) add(carried, 'div', 'Ambiguous hops: ' + count(r.ambiguousHops), 'scope-audit-caution');
      if (count(r.legacyObservations)) add(carried, 'div', 'Without frame header: ' + count(r.legacyObservations), 'scope-audit-caution');
      var split = cell(tr, 'Carrier / non-carrier');
      if (scope && r.carrier != null) {
        add(split, 'div', 'With ' + scope + ': ' + count(r.carrier));
        add(split, 'div', 'Without ' + scope + ': ' + count(r.nonCarrier));
      }
      add(split, 'div', count(r.forwarded) + ' forwarded', 'text-muted');
      var status = cell(tr, 'Transport evidence'), e = transportEvidence(r, scope, minForwarded);
      add(status, 'strong', e.title, e.caution ? 'scope-audit-caution' : null);
      if (e.detail) add(status, 'div', e.detail, 'text-muted');
      return tr;
    }
    function render() {
      var transport = state.view === 'transport';
      var filtered = (transport ? searchTransportRows : searchRows)(rows, state.query);
      var pages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
      if (state.page >= pages) { state.page = pages - 1; save(); }
      var visible = filtered.slice(state.page * PAGE_SIZE, (state.page + 1) * PAGE_SIZE);
      var keep = new Set();
      visible.forEach(function(r) {
        var key = String(r.publicKey || ''); keep.add(key);
        var signature = responseScope + '|' + JSON.stringify(r), cached = rowNodes.get(key);
        if (!cached || cached.signature !== signature) {
          var tr = (transport ? transportRowElement : rowElement)(r);
          if (cached) tbody.replaceChild(tr, cached.el);
          cached = { el: tr, signature: signature }; rowNodes.set(key, cached);
        }
      });
      rowNodes.forEach(function(cached, key) { if (!keep.has(key)) { cached.el.remove(); rowNodes.delete(key); } });
      visible.forEach(function(r, index) {
        var node = rowNodes.get(String(r.publicKey || '')).el;
        if (tbody.children[index] !== node) tbody.insertBefore(node, tbody.children[index] || null);
      });
      pageLabel.textContent = filtered.length + ' of ' + rows.length + ' repeaters · Page ' + (state.page + 1) + ' of ' + pages;
      previous.disabled = state.page === 0; next.disabled = state.page + 1 >= pages;
      if (!filtered.length) message.textContent = rows.length ? 'No repeaters match this search.' : transport ? 'No repeater was seen forwarding flood traffic in this window and region.' : 'No confirmed observer declarations available. An unknown declaration is not the same as an empty declaration.';
      else if (message.dataset.loaded) message.textContent = 'Observed forwarding in ' + state.window + '. Results are limited to available observer coverage.';
    }
    function showTransportSummary(data) {
      var s = data.summary || {}, p = data.population || {}, scope = responseScope;
      var text = count(s.total) + ' repeaters seen forwarding · ' + count(s.neverQueried) + ' never asked · ' + count(s.declared) + ' with a declaration';
      if (scope) {
        text += ' · ' + count(s.carrier) + ' carrying ' + scope + ' · ' + count(s.notSeenCarrying) + ' not seen carrying · ' + count(s.ambiguous) + ' ambiguous · ' +
          count(s.incomplete) + ' incomplete · ' + count(s.lowEvidence) + ' too little traffic' + (count(s.noScopeTraffic) ? ' · ' + count(s.noScopeTraffic) + ' without ' + scope + ' traffic' : '');
      }
      summary.textContent = text;
      var region = Array.isArray(data.region) ? data.region.map(String).join(', ') : '';
      var pop = 'Population: ' + count(p.transmissions) + ' flood transmissions, ' + count(p.observations) + ' observations' +
        (region ? ' heard by ' + count(p.regionObservers) + ' observer' + (count(p.regionObservers) === 1 ? '' : 's') + ' in ' + region : ' from all observers') +
        (scope && p.scopeTransmissions != null ? ' · ' + count(p.scopeTransmissions) + ' carried ' + scope : '') +
        ' · ' + count(p.ambiguousHops) + ' ambiguous hops credited to nobody.';
      var c = data.coverage || {};
      if (!c.complete) pop += c.oldestObservation ? ' Captures start at ' + String(c.oldestObservation) + ': the window is not fully covered.' : ' No stored observations: the window is not covered.';
      if (data.truncated) pop += ' Rows are truncated.';
      population.textContent = pop;
      scopeList.textContent = '';
      (data.scopes || []).forEach(function(sc) { var o = add(scopeList, 'option'); o.value = String(sc.name); });
    }
    async function load(w, bust) {
      state.window = windowValue(w);
      applyView();
      toolbar.querySelectorAll('[data-win]').forEach(function(b) { b.classList.toggle('active', b.dataset.win === state.window); });
      var token = ++generation;
      if (!active) return;
      var transport = state.view === 'transport';
      if (transport) save(); // a changed global region reaches the shareable link
      message.textContent = 'Loading scope audit…'; delete message.dataset.loaded;
      try {
        var path = transport ? transportPath(state, regionParam()) : '/scope-audit?window=' + encodeURIComponent(state.window);
        var data = await options.api(path, { ttl: 30000, bust: !!bust, retry503: false });
        if (token !== generation || !active || !container.isConnected) return;
        if (!data || data.error || !Array.isArray(data.rows)) throw new Error(data && data.error || 'Invalid audit response');
        rows = data.rows;
        if (transport) {
          responseScope = normalizeScope(data.scope); minForwarded = count(data.minForwarded);
          showTransportSummary(data);
        } else {
          var s = data.summary || {};
          summary.textContent = count(s.total) + ' confirmed declarations · ' + count(s.notObserved) + ' with unobserved scopes · ' + count(s.undeclaredObserved) + ' with undeclared scopes · ' + count(s.wildcardContradiction) + ' unscoped mismatches · ' + count(s.incomplete) + ' incomplete · ' + count(data.ambiguousHops) + ' ambiguous hop observations';
        }
        message.dataset.loaded = 'true'; render();
      } catch (err) {
        if (token !== generation || !active || !container.isConnected) return;
        rows = []; render(); summary.textContent = ''; population.textContent = '';
        message.textContent = 'Failed to load scope audit: ' + String(err.message || err) + '. Use Refresh to retry.';
      }
    }
    applyView();
    return { load: load, syncState: function(hash) { clearTimeout(searchTimer); state = stateFromHash(hash); generation++; input.value = state.query; scopeInput.value = state.scope; applyView(); render(); }, setActive: function(value) { active = value; generation++; }, destroy: function() { active = false; generation++; clearTimeout(searchTimer); } };
  }
  // Hash after a view-state change. Transport-only params are dropped in the declared view;
  // a present region= is written back so a copied transport link carries it (#531 syncs only a present region=).
  function hashWithState(hash, query, page, view, scope, region) {
    var parts = String(hash || '').split('?'); var params = new URLSearchParams(parts[1] || '');
    if (query) params.set('saq', query); else params.delete('saq');
    if (page) params.set('sap', String(page)); else params.delete('sap');
    if (view === 'transport') params.set('sav', view); else params.delete('sav');
    if (view === 'transport' && scope) params.set('scope', scope); else params.delete('scope');
    if (view === 'transport' && region) params.set('region', region);
    return parts[0] + (params.toString() ? '?' + params.toString() : '');
  }
  var exported = { mount: mount, hashWithState: hashWithState, windowValue: windowValue, stateFromHash: stateFromHash, searchRows: searchRows, searchTransportRows: searchTransportRows, declarationAge: declarationAge, assessment: assessment,
    normalizeScope: normalizeScope, transportPath: transportPath, transportEvidence: transportEvidence, pageSize: PAGE_SIZE };
  if (typeof module !== 'undefined' && module.exports) module.exports = exported;
  else root.ScopeAudit = exported;
})(typeof window !== 'undefined' ? window : globalThis);
