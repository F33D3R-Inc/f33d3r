async function submitReply(parentId) {
  const box = document.getElementById('reply-box');
  const errEl = document.getElementById('reply-error');
  const body = box?.value.trim();
  if (!body) return;
  const btn = event.currentTarget;
  btn.disabled = true; btn.textContent = 'Posting…';
  if (errEl) errEl.style.display = 'none';
  try {
    const fd = new FormData();
    fd.append('parent_id', parentId);
    fd.append('body', body);
    const r = await fetch('/api/reply', { method:'POST', body: fd, headers:{'HX-Request':'true'} });
    if (r.ok || r.status === 303) {
      // Reload just the replies section by reloading page (lightweight)
      window.location.reload();
    } else {
      const t = await r.text();
      if (errEl) { errEl.textContent = t || 'Reply failed'; errEl.style.display = ''; }
      btn.disabled = false; btn.textContent = 'Reply';
    }
  } catch(e) {
    if (errEl) { errEl.textContent = 'Network error — try again'; errEl.style.display = ''; }
    btn.disabled = false; btn.textContent = 'Reply';
  }
}
// Char counter for reply box
document.getElementById('reply-box')?.addEventListener('input', function() {
  const counter = document.getElementById('reply-char-count');
  if (counter) counter.textContent = String(25000 - this.value.length);
});
