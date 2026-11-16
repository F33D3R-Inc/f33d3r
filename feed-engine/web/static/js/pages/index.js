// Home feed page. Registered as a Shell page: executed once per session, the
// mount runs on every arrival of the home Playground and returns a teardown
// that releases what this page held.
//
// What a reader looks at is measured by the view_time event on /events, which
// the server records and queues for AethyrRank. This page sends no signals.
F33D3R.page('index', function (root) {
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

  return function teardown() {};
});
