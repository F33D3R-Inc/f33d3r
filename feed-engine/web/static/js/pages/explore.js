// Server values from <meta> tags — no template injection in JS
const SESSION_ID      = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
const MY_PIAL         = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE       = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';

// Surface name → works feed surface param
const SURF_MAP = {
  trending:  'trending',
  you:       'for_you',
  news:      'trending',
  live:      'trending',
  paid:      'for_you',
};

function setExploreSurf(surf, btn) {
  document.querySelectorAll('.explore-surf-tab').forEach(t => t.classList.remove('active'));
  btn.classList.add('active');

  const strip      = document.getElementById('trending-strip');
  const tagsPanel  = document.getElementById('explore-tags-panel');
  const feedResult = document.getElementById('explore-results');

  // Tags tab: show tag browser, hide feed + trending strip
  if (surf === 'tags') {
    if (strip)      strip.style.display      = 'none';
    if (tagsPanel)  tagsPanel.style.display  = '';
    if (feedResult) feedResult.style.display = 'none';
    return;
  }

  // All other tabs: hide tags panel, show feed
  if (tagsPanel)  tagsPanel.style.display  = 'none';
  if (feedResult) feedResult.style.display = '';

  // Trending strip only visible on the Trending tab
  if (strip) strip.style.display = surf === 'trending' ? '' : 'none';

  const feedSurface = SURF_MAP[surf] || 'trending';
  htmx.ajax('GET', '/facets/works/feed?surface=' + feedSurface,
    { target: '#explore-results', swap: 'innerHTML' });
}

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
