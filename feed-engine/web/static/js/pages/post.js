// Post page FA Live — registers the focal post for SSE watching and handles live events.
// Runs only on the post detail page. Wires two SSE callbacks:
//   _onSSENewReply       — appends a pre-rendered post_card to #thread-container
//   _onSSEPostEngagement — handled by the global default in f33d3r.js (id-based HTML swap)
//
// Watch registration tells the server to push new_reply and post_engagement events
// for this post to the current user's SSE session.
(function() {
  'use strict';

  var postEl = document.querySelector('[data-watch-post]');
  if (!postEl) return;
  var postId = postEl.dataset.watchPost;
  if (!postId) return;

  // Register with the SSE session so the server knows to push events for this post.
  function registerWatch(action) {
    fetch('/api/events/watch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'type=post&id=' + encodeURIComponent(postId) + '&action=' + action,
      keepalive: action === 'unwatch'
    }).catch(function() {});
  }

  // Register immediately and unregister on leave.
  registerWatch('watch');
  window.addEventListener('beforeunload', function() { registerWatch('unwatch'); });
  document.addEventListener('htmx:beforeRequest', function() { registerWatch('unwatch'); });

  // Listen for new replies — data is JSON {"post_id":"...","html":"<post_card html>"}.
  // Appends the pre-rendered post_card to #thread-container so new replies appear live
  // without any polling or full reload.
  window._onSSENewReply = function(e) {
    if (!e.data) return;
    try {
      var data = JSON.parse(e.data);
      if (data.post_id !== postId) return;
      var container = document.getElementById('thread-container');
      if (!container) return;
      var wrapper = document.createElement('div');
      wrapper.innerHTML = data.html;
      while (wrapper.firstChild) {
        container.appendChild(wrapper.firstChild);
      }
      if (typeof formatPostTimes === 'function') formatPostTimes();
    } catch (err) {}
  };

})();
