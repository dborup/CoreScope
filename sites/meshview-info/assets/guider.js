// Click-to-play for /guider: nothing is loaded from YouTube until the visitor presses play.
// A card without a valid video id shows "Video på vej" and is not clickable.
(function () {
  'use strict';
  var ID = /^[A-Za-z0-9_-]{11}$/;

  function embedUrl(id) {
    return 'https://www.youtube-nocookie.com/embed/' + id + '?autoplay=1&rel=0&modestbranding=1&cc_load_policy=1&hl=da';
  }

  function play(btn, id) {
    var frame = document.createElement('iframe');
    frame.src = embedUrl(id);
    frame.title = btn.getAttribute('data-title') || 'Video';
    frame.setAttribute('allow', 'autoplay; encrypted-media; picture-in-picture; fullscreen');
    frame.setAttribute('allowfullscreen', '');
    frame.setAttribute('referrerpolicy', 'strict-origin-when-cross-origin');
    btn.parentNode.replaceChild(frame, btn);
    frame.focus();
  }

  function setup(card) {
    var btn = card.querySelector('.guide-play');
    if (!btn) return;
    var id = (card.getAttribute('data-youtube') || '').trim();
    var title = btn.getAttribute('data-title') || 'video';
    if (!ID.test(id)) {
      btn.disabled = true;
      btn.setAttribute('aria-label', title + ': video på vej');
      var soon = document.createElement('span');
      soon.className = 'guide-badge soon';
      soon.textContent = 'Video på vej';
      btn.appendChild(soon);
      return;
    }
    btn.setAttribute('aria-label', 'Afspil video: ' + title);
    btn.addEventListener('click', function () { play(btn, id); });
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { ID: ID, embedUrl: embedUrl };
    return;
  }
  var cards = document.querySelectorAll('.guide-card');
  for (var i = 0; i < cards.length; i++) setup(cards[i]);
})();
