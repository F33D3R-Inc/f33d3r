F33D3R.page('bookmarks', function () {
  function filterBookmarks(btn, type) {
    document.querySelectorAll('.bookmarks-tab').forEach(t => t.classList.remove('is-active'));
    btn.classList.add('is-active');
    document.querySelectorAll('#bm-list .bookmark-card').forEach(item => {
      item.style.display = (type === 'all' || item.dataset.type === type) ? '' : 'none';
    });
  }
  window.filterBookmarks = filterBookmarks;
});
