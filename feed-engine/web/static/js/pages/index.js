// Server values from <meta> tags — no template injection in JS
const SESSION_ID    = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
const MY_PIAL       = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE     = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';

function switchSurface(btn, surface) {
  document.querySelectorAll('.surface-tab').forEach(t => t.classList.remove('active'));
  btn.classList.add('active');
  htmx.ajax('GET', `/feed?surface=${surface}&session_id=${SESSION_ID}`, {target:'#feed-container',swap:'innerHTML'});
}
const observer = new IntersectionObserver(entries => {
  entries.forEach(e => {
    if (e.isIntersecting) { e.target._dwellStart = Date.now(); }
    else if (e.target._dwellStart) {
      const ms = Date.now() - e.target._dwellStart;
      if (ms > 2000) {
        navigator.sendBeacon('/api/feedback', JSON.stringify({
          user_id:'demo_user', session_id:SESSION_ID, surface:CURRENT_SURFACE,
          events:[{content_id:e.target.dataset.contentId,event_type:'view_complete',
            position_at_display:parseInt(e.target.dataset.rank)||0,
            timestamp:new Date().toISOString(),dwell_ms:ms,exploration_slot:false}]
        }));
      }
      delete e.target._dwellStart;
    }
  });
},{threshold:0.8});
document.addEventListener('htmx:afterSwap', ev => {
  ev.detail.target.querySelectorAll('.post-card').forEach(el => observer.observe(el));
});
