F33D3R.page('notifications', function () {
  function filterNotifs(btn, type) {
    document.querySelectorAll('.notif-tab').forEach(t => t.classList.remove('active'));
    btn.classList.add('active');
    document.querySelectorAll('.notif-item').forEach(item => {
      item.style.display = (type === 'all' || item.dataset.type === type) ? '' : 'none';
    });
  }
  async function markAllRead() {
    await fetch('/api/notifications/read-all', { method:'POST', headers:{'HX-Request':'true'} });
    document.querySelectorAll('.notif-item .notif-unread').forEach(el => el.classList.remove('notif-unread'));
    document.querySelectorAll('[style*="border-radius:50%;background:var(--accent)"]').forEach(dot => dot.style.display='none');
    toast('All notifications marked as read');
  }
  window.filterNotifs = filterNotifs;
  window.markAllRead  = markAllRead;
});
