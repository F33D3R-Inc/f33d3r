// Home feed page. Registered as a Shell page: executed once per session, the
// mount runs on every arrival of the home Playground and returns a teardown
// that releases what this page held.
F33D3R.page('index', function (root) {
  // Server values from <meta> tags — no template injection in JS
  const SESSION_ID      = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
  const MY_PIAL         = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
  const MY_HANDLE       = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
  const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';

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
  };
});
