F33D3R.page('explore', function () {
  // The Explore canvas — tab strip, sections, results — is a server Facet
  // (GET /facets/explore/canvas). This page owns only the list/grid view
  // preference for the results container.
  function setExploreView(v) {
    const feed = document.getElementById('explore-results');
    const lb   = document.getElementById('explore-list-btn');
    const gb   = document.getElementById('explore-grid-btn');
    if (!feed) return;
    if (v === 'grid') {
      feed.classList.add('explore-grid-view');
      if (lb) { lb.style.background = 'transparent'; lb.style.borderColor = 'var(--border)'; lb.style.color = 'var(--text-muted)'; }
      if (gb) { gb.style.background = 'color-mix(in srgb,var(--accent) 12%,transparent)'; gb.style.borderColor = 'var(--accent)'; gb.style.color = 'var(--accent)'; }
    } else {
      feed.classList.remove('explore-grid-view');
      if (lb) { lb.style.background = 'color-mix(in srgb,var(--accent) 12%,transparent)'; lb.style.borderColor = 'var(--accent)'; lb.style.color = 'var(--accent)'; }
      if (gb) { gb.style.background = 'transparent'; gb.style.borderColor = 'var(--border)'; gb.style.color = 'var(--text-muted)'; }
    }
  }

  window.setExploreView = setExploreView;
});
