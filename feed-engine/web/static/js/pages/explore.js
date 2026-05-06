// Server values from <meta> tags — no template injection in JS
const SESSION_ID    = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
const MY_PIAL       = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE     = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';

function setExploreView(v) {
  const feed = document.getElementById('explore-results');
  const lb   = document.getElementById('explore-list-btn');
  const gb   = document.getElementById('explore-grid-btn');
  if (!feed) return;
  if (v === 'grid') {
    feed.classList.add('explore-grid-view');
    lb.style.cssText = lb.style.cssText.replace('var(--accent)','transparent').replace(/color:[^;]+;/,'color:var(--text-muted);').replace(/border:[^;]+;/,'border:1px solid var(--border);');
    gb.style.cssText = gb.style.cssText + 'background:color-mix(in srgb,var(--accent) 12%,transparent);border-color:var(--accent);color:var(--accent)';
  } else {
    feed.classList.remove('explore-grid-view');
    lb.style.background = 'color-mix(in srgb,var(--accent) 12%,transparent)';
    lb.style.borderColor = 'var(--accent)'; lb.style.color = 'var(--accent)';
    gb.style.background = 'transparent'; gb.style.borderColor = 'var(--border)'; gb.style.color = 'var(--text-muted)';
  }
}
function setExploreSurf(surf, btn) {
  document.querySelectorAll('.explore-surf-tab').forEach(t => t.classList.remove('active'));
  btn.classList.add('active');
  const strip = document.getElementById('trending-strip');
  if (strip) strip.style.display = surf === 'trending' ? '' : 'none';
  const surfMap = { discovery:'explore', trending:'trending', latest:'latest' };
  htmx.ajax('GET', '/feed?surface=' + (surfMap[surf]||'explore') + '&session_id=' + _exploreSessionID,
    { target:'#explore-results', swap:'innerHTML' });
}
