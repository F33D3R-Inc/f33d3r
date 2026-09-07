// Home feed page. Registered as a Shell page: executed once per session, the
// mount runs on every arrival of the home Playground and returns a teardown
// that releases what this page held.
F33D3R.page('index', function (root) {
  // Server values from <meta> tags — no template injection in JS
  const SESSION_ID      = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
  const MY_PIAL         = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
  const MY_HANDLE       = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
  const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';

  // ── New-post banner ───────────────────────────────────────────────────────────
  var _newPostCount = 0;

  function _updateBanner() {
    var banner = document.getElementById('new-post-banner');
    var label  = document.getElementById('new-post-count');
    if (!banner || !label) return;
    var activeTab = document.querySelector('.feed-tab.is-active');
    var surface   = activeTab ? activeTab.dataset.surface : '';
    if (_newPostCount > 0 && surface === 'following') {
      label.textContent = 'Show ' + _newPostCount + ' new post' + (_newPostCount === 1 ? '' : 's');
      banner.style.display = 'flex';
    } else {
      banner.style.display = 'none';
    }
  }

  // The Shell runtime (f33d3r.js) owns the default of both of these; they are
  // taken over while the home feed is on screen and handed back on teardown.
  var prevOnSSEPost   = window._onSSEPost;
  var prevShowNewPosts = window.showNewPosts;

  window._onSSEPost = function(e) {
    var n = parseInt(e.data, 10) || 0;
    if (n <= 0) return;
    _newPostCount += n;
    _updateBanner();
  };

  window.showNewPosts = function() {
    _newPostCount = 0;
    var banner = document.getElementById('new-post-banner');
    if (banner) banner.style.display = 'none';
    var fc = document.getElementById('feed-container');
    if (fc) htmx.trigger(fc, 'refreshFeed');
    var col = document.getElementById('main-col');
    if (col) col.scrollTo({ top: 0, behavior: 'smooth' });
  };

  // A feed tab is a hypermedia control (hx-get on the tab itself); the server
  // answers with the cards and the tab strip re-rendered. The banner only needs
  // to learn which tab is now active once that strip has been placed.
  F33D3R.once('index:tab-swap', function () {
    document.addEventListener('htmx:oobAfterSwap', function (e) {
      if (e.target && e.target.id === 'feed-tabs-scroll') _updateBanner();
    });
  });

  // ── Surface sheet (Add+ button) ───────────────────────────────────────────────
  function openTopicSheet() {
    var el = document.getElementById('topic-sheet');
    if (el) { el.classList.remove('hidden'); document.body.style.overflow = 'hidden'; }
  }
  function closeTopicSheet() {
    var el = document.getElementById('topic-sheet');
    if (el) { el.classList.add('hidden'); document.body.style.overflow = ''; }
  }
  function openAddTopic() { openTopicSheet(); }

  window.openTopicSheet  = openTopicSheet;
  window.closeTopicSheet = closeTopicSheet;
  window.openAddTopic    = openAddTopic;

  // Close on Escape — the topic sheet is a Shell overlay, so this is installed once.
  F33D3R.once('index:escape', function () {
    document.addEventListener('keydown', function(e) {
      if (e.key === 'Escape' && window.closeTopicSheet) window.closeTopicSheet();
    });
  });

  // ── Dwell tracking ────────────────────────────────────────────────────────────
  const observer = new IntersectionObserver(entries => {
    entries.forEach(e => {
      if (e.isIntersecting) { e.target._dwellStart = Date.now(); }
      else if (e.target._dwellStart) {
        const ms = Date.now() - e.target._dwellStart;
        if (ms > 2000) {
          navigator.sendBeacon('/api/feedback', JSON.stringify({
            user_id: MY_PIAL || 'anon', session_id: SESSION_ID, surface: CURRENT_SURFACE,
            events: [{
              content_id: e.target.dataset.contentId, event_type: 'view_complete',
              position_at_display: parseInt(e.target.dataset.rank) || 0,
              timestamp: new Date().toISOString(), dwell_ms: ms, exploration_slot: false
            }]
          }));
        }
        delete e.target._dwellStart;
      }
    });
  }, { threshold: 0.8 });

  function observeCards(scope) {
    if (!scope || !scope.querySelectorAll) return;
    scope.querySelectorAll('.post-card').forEach(el => observer.observe(el));
  }
  observeCards(root);

  function onSwap(ev) {
    var target = ev.detail && ev.detail.target;
    if (!target || !root.contains(target)) return;
    observeCards(target);
  }
  document.addEventListener('htmx:afterSwap', onSwap);

  return function teardown() {
    document.removeEventListener('htmx:afterSwap', onSwap);
    observer.disconnect();
    window._onSSEPost   = prevOnSSEPost;
    window.showNewPosts = prevShowNewPosts;
  };
});
