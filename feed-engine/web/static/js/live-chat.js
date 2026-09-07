// live-chat.js — FA Live watch surface · Fragment application.
//
// Chat lines, viewer tallies and stream status all arrive from the server as
// finished HTML Fragments on the existing /api/events stream. Nothing here
// builds markup, orders messages or counts anything: it locates the target by
// data-facet-id and puts the server's Fragment in place.
//
// Loaded by more than one page (home, profile, live). The Shell executes this
// file once per session; the document-level plumbing below is installed by
// that single execution, and the mount registered at the bottom pins the log
// of whichever Playground arrives.

(function () {
  'use strict';

  if (window.__F33D3R_LIVE_CHAT__) return;
  window.__F33D3R_LIVE_CHAT__ = true;

  var PIN_SLACK = 60; // px from the bottom that still counts as "following the log"

  function parseFragment(html) {
    var wrap = document.createElement('div');
    wrap.innerHTML = (html || '').trim();
    return wrap.firstElementChild;
  }

  function atBottom(log) {
    return (log.scrollHeight - log.scrollTop - log.clientHeight) <= PIN_SLACK;
  }

  // live_chat — one rendered live_chat_row appended to the log of its stream.
  window._onSSELiveChat = function (e) {
    if (!e.data) return;
    var row = parseFragment(e.data);
    if (!row) return;
    var streamID = row.getAttribute('data-stream-id');
    if (!streamID) return;

    var panel = document.querySelector('.live-chat[data-stream-id="' + streamID + '"]');
    if (!panel) return; // this viewer is not on that stream's surface
    var log = panel.querySelector('#live-chat-log') || panel.querySelector('.live-chat__log');
    if (!log) return;

    // The server already refused duplicates; this only guards a reconnect replay.
    var id = row.getAttribute('data-facet-id');
    if (id && log.querySelector('[data-facet-id="' + id + '"]')) return;

    var placeholder = log.querySelector('.live-chat__empty');
    if (placeholder) placeholder.remove();

    var follow = atBottom(log);
    log.appendChild(row);
    if (typeof htmx !== 'undefined') htmx.process(row);
    if (follow) log.scrollTop = log.scrollHeight;
  };

  // live_facet — a re-rendered live Facet (viewer tally, LIVE badge, player,
  // feed card). Every element carrying that facet id is replaced, so the same
  // stream shown twice on one page stays in step.
  window._onSSELiveFacet = function (e) {
    if (!e.data) return;
    var neo = parseFragment(e.data);
    if (!neo) return;
    var facetID = neo.getAttribute('data-facet-id');
    if (!facetID) return;

    document.querySelectorAll('[data-facet-id="' + facetID + '"]').forEach(function (old) {
      var frag = neo.cloneNode(true);
      old.replaceWith(frag);
      if (typeof htmx !== 'undefined') htmx.process(frag);
      if (window.F33D3R_LivePlayer) {
        var v = frag.matches && frag.matches('video[data-f33d-live-hls]')
          ? frag
          : (frag.querySelector && frag.querySelector('video[data-f33d-live-hls]'));
        if (v) window.F33D3R_LivePlayer.attach(v);
      }
    });
  };

  // Open the log at the newest line, and keep it there after an htmx swap of
  // the panel (the composer's own response lands via hx-swap="beforeend").
  function pin(scope) {
    var logs = (scope || document).querySelectorAll('.live-chat__log');
    logs.forEach(function (log) { log.scrollTop = log.scrollHeight; });
  }

  document.body.addEventListener('htmx:afterSwap', function (e) {
    var t = e.detail && e.detail.target;
    if (t && (t.id === 'live-chat-log' || (t.closest && t.closest('.live-chat')))) {
      var log = document.getElementById('live-chat-log');
      if (log) log.scrollTop = log.scrollHeight;
    }
  });

  // Every arrival of a Playground carrying a chat log opens it at the newest line.
  window.F33D3R.page('live-chat', function (root) {
    pin(root && root.querySelectorAll ? root : document);
  });
})();
