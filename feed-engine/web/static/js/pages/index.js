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

function switchSurface(btn, surface) {
  document.querySelectorAll('.feed-tab, .tab').forEach(t => t.classList.remove('is-active'));
  btn.classList.add('is-active');
  htmx.ajax('GET', `/facets/works/feed?surface=${surface}`, {target:'#feed-container',swap:'innerHTML'});
  _updateBanner();
}

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

// Close on Escape
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') closeTopicSheet();
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

document.addEventListener('htmx:afterSwap', ev => {
  ev.detail.target.querySelectorAll('.post-card').forEach(el => observer.observe(el));
});
