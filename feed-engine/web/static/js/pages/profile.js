// profile.js — profile page actions. Registered as a Shell page: executed once
// per session, the mount runs on every arrival of a profile Playground.
F33D3R.page('profile', function (root) {
  // ── Online presence indicator ─────────────────────────────────────────────────
  (function() {
    var dot = root.querySelector ? root.querySelector('#profile-online-dot') : document.getElementById('profile-online-dot');
    if (!dot) return;
    var handle = dot.dataset.handle;
    if (!handle) return;
    fetch('/api/online/' + encodeURIComponent(handle))
      .then(function(r) { return r.json(); })
      .then(function(d) {
        if (!dot.isConnected) return;
        dot.style.display = 'block';
        dot.style.background = d.online ? '#22c55e' : '#6b7280';
        dot.title = d.online ? 'Online now' : 'Offline';
      })
      .catch(function() {});
  })();

  let _profileView = 'list';
  function setProfileView(v) {
    _profileView = v;
    const feed = document.getElementById('profile-feed');
    const lb   = document.getElementById('view-list-btn');
    const gb   = document.getElementById('view-grid-btn');
    if (!feed) return;
    if (v === 'grid') {
      feed.classList.add('grid-view');
      if(lb) { lb.style.background='transparent'; lb.style.border='1px solid var(--border)'; lb.style.color='var(--text-muted)'; }
      if(gb) { gb.style.background='color-mix(in srgb,var(--accent) 15%,transparent)'; gb.style.border='1px solid var(--accent)'; gb.style.color='var(--accent)'; }
    } else {
      feed.classList.remove('grid-view');
      if(lb) { lb.style.background='color-mix(in srgb,var(--accent) 15%,transparent)'; lb.style.border='1px solid var(--accent)'; lb.style.color='var(--accent)'; }
      if(gb) { gb.style.background='transparent'; gb.style.border='1px solid var(--border)'; gb.style.color='var(--text-muted)'; }
    }
  }

  // Subscriptions are server Facets: btn_subscribe opens subscribe_modal from
  // /facets/subscribe_modal, and the server redraws the page after a change.

  window.setProfileView      = setProfileView;
});
