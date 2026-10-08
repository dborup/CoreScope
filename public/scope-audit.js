/* Scope Audit: one bulk request, local search, and a bounded keyed row diff. */
(function(root) {
  'use strict';
  var PAGE_SIZE = 50;
  function windowValue(value) { return ['1h', '24h', '7d'].indexOf(value) >= 0 ? value : '24h'; }
  function stateFromHash(hash) {
    var p = new URLSearchParams(String(hash || '').split('?')[1] || '');
    return { window: windowValue(p.get('swin')), query: p.get('saq') || '', page: Math.max(0, Math.floor(Number(p.get('sap')) || 0)) };
  }
  function searchRows(rows, query) {
    var q = String(query || '').trim().toLowerCase();
    return rows.filter(function(r) {
      return !q || [r.name, r.publicKey, r.configuredScope].concat(r.declaredScopes || []).join(' ').toLowerCase().indexOf(q) >= 0;
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
  function mount(container, options) {
    var doc = container.ownerDocument;
    var rows = [], rowNodes = new Map(), generation = 0, active = true, searchTimer = null;
    var state = stateFromHash(options.hash());
    function element(tag, text, cls) {
      var el = doc.createElement(tag);
      if (text != null) el.textContent = String(text);
      if (cls) el.className = cls;
      return el;
    }
    function add(parent, tag, text, cls) { var el = element(tag, text, cls); parent.appendChild(el); return el; }
    add(container, 'h3', 'Scope Audit');
    add(container, 'p', 'Whole-network audit. The audit window is independent of the global time and region filters.', 'text-muted');
    add(container, 'p', 'Compares confirmed observer declarations with forwarding observed in the selected window. A declared scope without observed traffic does not prove a configuration fault. Observer coverage, quiet regions and configuration changes can explain gaps.', 'text-muted');
    add(container, 'p', 'Short windows may contain little traffic; 7d may span configuration changes. Declarations older than 7d or with an unknown timestamp, unknown scopes and ambiguous hop attribution make the evidence incomplete.', 'text-muted');
    var toolbar = add(container, 'div', null, 'scope-audit-controls');
    ['1h', '24h', '7d'].forEach(function(w) {
      var btn = add(toolbar, 'button', w, 'tab-btn'); btn.dataset.win = w; btn.dataset.auditWindow = 'true';
      btn.addEventListener('click', function() { options.onWindow(w); });
    });
    var label = add(toolbar, 'label', 'Search repeaters / scopes ');
    var input = add(label, 'input'); input.type = 'search'; input.id = 'scope-audit-search'; input.value = state.query;
    input.placeholder = 'Name, public key or scope';
    var refresh = add(toolbar, 'button', 'Refresh', 'tab-btn');
    refresh.addEventListener('click', function() { load(state.window, true); });
    var message = add(container, 'p', '', 'text-muted'); message.id = 'scope-audit-message'; message.setAttribute('role', 'status');
    var summary = add(container, 'p'); summary.id = 'scope-audit-summary';
    var wrap = add(container, 'div', null, 'scope-audit-table-wrap');
    var table = add(wrap, 'table', null, 'data-table scope-audit-table'); table.id = 'scope-audit-table';
    var header = add(add(table, 'thead'), 'tr');
    ['Repeater', 'Declaration / age', 'Declared scopes', 'Observed forwarding', 'Assessment'].forEach(function(t) { add(header, 'th', t); });
    var tbody = add(table, 'tbody'); tbody.id = 'scope-audit-rows';
    var pager = add(container, 'div', null, 'scope-audit-controls');
    var previous = add(pager, 'button', 'Previous', 'tab-btn');
    var pageLabel = add(pager, 'span');
    var next = add(pager, 'button', 'Next', 'tab-btn');
    function save() { options.onState(state.query, state.page); }
    previous.addEventListener('click', function() { state.page = Math.max(0, state.page - 1); save(); render(); });
    next.addEventListener('click', function() { state.page++; save(); render(); });
    input.addEventListener('input', function() {
      if (!active || !container.isConnected) return;
      clearTimeout(searchTimer);
      state.query = input.value; state.page = 0; save();
      searchTimer = setTimeout(function() { if (active && container.isConnected) render(); }, 150);
    });
    function rowElement(r) {
      var tr = element('tr'); tr.dataset.publicKey = String(r.publicKey || '');
      var name = add(tr, 'td');
      var link = add(name, 'a', r.name || String(r.publicKey || '').slice(0, 16));
      link.href = '#/nodes/' + encodeURIComponent(String(r.publicKey || ''));
      add(name, 'div', r.role || 'Repeater', 'text-muted');
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
    function render() {
      var filtered = searchRows(rows, state.query);
      var pages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
      if (state.page >= pages) { state.page = pages - 1; save(); }
      var visible = filtered.slice(state.page * PAGE_SIZE, (state.page + 1) * PAGE_SIZE);
      var keep = new Set();
      visible.forEach(function(r) {
        var key = String(r.publicKey || ''); keep.add(key);
        var signature = JSON.stringify(r), cached = rowNodes.get(key);
        if (!cached || cached.signature !== signature) {
          var tr = rowElement(r);
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
      if (!filtered.length) message.textContent = rows.length ? 'No repeaters match this search.' : 'No confirmed observer declarations available. An unknown declaration is not the same as an empty declaration.';
      else if (message.dataset.loaded) message.textContent = 'Observed forwarding in ' + state.window + '. Results are limited to available observer coverage.';
    }
    async function load(w, bust) {
      state.window = windowValue(w);
      toolbar.querySelectorAll('[data-win]').forEach(function(b) { b.classList.toggle('active', b.dataset.win === state.window); });
      var token = ++generation;
      if (!active) return;
      message.textContent = 'Loading scope audit…'; delete message.dataset.loaded;
      try {
        var data = await options.api('/scope-audit?window=' + encodeURIComponent(state.window), { ttl: 30000, bust: !!bust, retry503: false });
        if (token !== generation || !active || !container.isConnected) return;
        if (!data || data.error || !Array.isArray(data.rows)) throw new Error(data && data.error || 'Invalid audit response');
        rows = data.rows;
        var s = data.summary || {};
        summary.textContent = count(s.total) + ' confirmed declarations · ' + count(s.notObserved) + ' with unobserved scopes · ' + count(s.undeclaredObserved) + ' with undeclared scopes · ' + count(s.wildcardContradiction) + ' unscoped mismatches · ' + count(s.incomplete) + ' incomplete · ' + count(data.ambiguousHops) + ' ambiguous hop observations';
        message.dataset.loaded = 'true'; render();
      } catch (err) {
        if (token !== generation || !active || !container.isConnected) return;
        rows = []; render(); summary.textContent = '';
        message.textContent = 'Failed to load scope audit: ' + String(err.message || err) + '. Use Refresh to retry.';
      }
    }
    return { load: load, syncState: function(hash) { clearTimeout(searchTimer); state = stateFromHash(hash); input.value = state.query; render(); }, setActive: function(value) { active = value; generation++; }, destroy: function() { active = false; generation++; clearTimeout(searchTimer); } };
  }
  var exported = { mount: mount, windowValue: windowValue, stateFromHash: stateFromHash, searchRows: searchRows, declarationAge: declarationAge, assessment: assessment, pageSize: PAGE_SIZE };
  if (typeof module !== 'undefined' && module.exports) module.exports = exported;
  else root.ScopeAudit = exported;
})(typeof window !== 'undefined' ? window : globalThis);
