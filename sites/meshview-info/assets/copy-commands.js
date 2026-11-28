(function () {
  'use strict';

  function fallbackCopy(text) {
    var previous = document.activeElement;
    var textarea = document.createElement('textarea');
    textarea.value = text;
    textarea.setAttribute('readonly', '');
    textarea.setAttribute('aria-label', 'Kommandoer til kopiering');
    textarea.style.position = 'fixed';
    textarea.style.opacity = '0';
    textarea.style.fontSize = '16px';
    document.body.appendChild(textarea);
    try {
      textarea.focus();
      textarea.select();
      textarea.setSelectionRange(0, text.length);
      return document.execCommand('copy') === true;
    } catch (_error) {
      return false;
    } finally {
      textarea.remove();
      if (previous && typeof previous.focus === 'function') previous.focus({ preventScroll: true });
    }
  }

  function initialize() {
    document.querySelectorAll('.code-block').forEach(function (block, index) {
      var code = block.querySelector('pre code');
      var bar = block.querySelector('.code-bar');
      if (!code || !bar || block.querySelector('.copy-commands')) return;
      var button = document.createElement('button');
      var status = document.createElement('span');
      var resetTimer;
      button.type = 'button';
      button.className = 'copy-commands';
      button.textContent = 'Kopiér';
      button.setAttribute('aria-label', 'Kopiér kommandoerne i blok ' + (index + 1));
      status.className = 'copy-status';
      status.setAttribute('role', 'status');
      status.setAttribute('aria-live', 'polite');
      status.setAttribute('aria-atomic', 'true');
      bar.appendChild(button);
      block.appendChild(status);

      button.addEventListener('click', async function () {
        if (button.disabled) return;
        clearTimeout(resetTimer);
        button.disabled = true;
        status.textContent = '';
        var text = code.textContent;
        var copied = false;
        try {
          if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
            await navigator.clipboard.writeText(text);
            copied = true;
          }
        } catch (_error) {
          // Clipboard permission can be denied; try the selection-based fallback.
        }
        if (!copied) copied = fallbackCopy(text);
        button.disabled = false;
        button.textContent = copied ? 'Kopieret!' : 'Prøv igen';
        status.textContent = copied ? 'Kommandoerne er kopieret.' : 'Kunne ikke kopiere. Markér og kopiér kommandoerne manuelt.';
        resetTimer = setTimeout(function () { button.textContent = 'Kopiér'; }, 3000);
      });
    });
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', initialize, { once: true });
  else initialize();
}());
