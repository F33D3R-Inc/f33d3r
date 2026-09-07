'use strict';

// Admin page — left-nav active highlight.
// Registered as a Shell page: the file executes once per session and this
// mount runs each time the admin Playground arrives.
F33D3R.page('admin', function () {
  function activateByPath(path) {
    var items = document.querySelectorAll('.admin-nav-item');
    items.forEach(function(item) {
      // hx-get="/admin/partials/users" → extract "users"
      var hxGet = item.getAttribute('hx-get') || '';
      var seg = hxGet.replace('/admin/partials/', '').split('?')[0];
      item.classList.toggle('is-active', path.indexOf(seg) !== -1 && seg !== '');
    });
  }

  // On any HTMX request fired into #admin-content, sync the nav highlight.
  // Document-level: installed once, whichever arrival of the page does it.
  F33D3R.once('admin:nav-sync', function () {
    document.body.addEventListener('htmx:beforeRequest', function(e) {
      var target = e.detail && e.detail.target;
      if (!target || target.id !== 'admin-content') return;
      var path = (e.detail.requestConfig && e.detail.requestConfig.path) || '';
      activateByPath(path);
    });
  });

  // Expose for inline onclick="adminNavActivate(this)" calls from _admin_nav.html.
  window.adminNavActivate = function(el) {
    document.querySelectorAll('.admin-nav-item').forEach(function(i) {
      i.classList.remove('is-active');
    });
    el.classList.add('is-active');
  };
});
