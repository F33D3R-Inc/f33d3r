// vision-viewer.js — Vision lane read surface · navigation and media wiring only.
//
// This file owns nothing. The sequence, the order, the progress, the ring state
// and where "next" leads are all decided on the server and arrive as rendered
// Fragments; every step is a round trip. What happens here is input plumbing:
// a key, a swipe or a finished video is translated into activating the anchor
// the server already rendered, and focus is moved so the surface is reachable
// without a pointer.
//
// Nothing below reads or writes application state, and nothing below decides
// what a Vision looks like.

(function () {
  'use strict';

  if (window.__F33D3R_VISION_VIEWER__) return;
  window.__F33D3R_VISION_VIEWER__ = true;

  var SLOT = 'vision-viewer-slot';
  var SWIPE_MIN = 44;   // px before a drag counts as a swipe
  var SWIPE_SLOPE = 1.2; // how much one axis must beat the other

  function slot() { return document.getElementById(SLOT); }

  function viewer() {
    var s = slot();
    return s ? s.querySelector('[data-vision-viewer]') : null;
  }

  function step(sel) {
    var v = viewer();
    var el = v && v.querySelector(sel);
    if (el) { el.click(); return true; }
    return false;
  }

  // ── open / close ───────────────────────────────────────────────────────────

  // The server sends the frame; this moves focus onto it and stops the surface
  // underneath from scrolling behind the overlay.
  function onFrameArrived(v) {
    document.documentElement.classList.add('vision-viewer-open');
    if (typeof v.focus === 'function') v.focus();
    playFrameVideo(v);
  }

  // Closing empties the slot, announces the close, and puts focus back on the
  // avatar the sequence was opened from. No ring is restyled here: the announce
  // is what every ring surface on the page listens for, and each answers it by
  // asking the server for its own Fragment again.
  function closeViewer() {
    var v = viewer();
    var returnID = v ? v.getAttribute('data-vision-return') : '';
    var s = slot();
    if (s) s.innerHTML = '';
    document.documentElement.classList.remove('vision-viewer-open');
    announceClosed();
    var back = returnID ? document.getElementById(returnID) : null;
    if (back && typeof back.focus === 'function') { back.focus(); return; }
    var rail = document.getElementById('vision-rail-slot');
    var first = rail && rail.querySelector('.vision-ring');
    if (first && typeof first.focus === 'function') first.focus();
  }

  // The close signal. Ring surfaces carry hx-trigger="visionclosed from:body";
  // the server decides what each of them then says.
  function announceClosed() {
    if (!document.body) return;
    document.body.dispatchEvent(new CustomEvent('visionclosed', { bubbles: false }));
  }

  // ── media ──────────────────────────────────────────────────────────────────

  // A frame's video plays on arrival and, when it finishes, asks the server for
  // the next frame. Advancing is still the server's answer — this only presses
  // the anchor the server rendered.
  function playFrameVideo(v) {
    var vid = v.querySelector('video');
    if (!vid) return;
    vid.loop = false;
    vid.addEventListener('ended', function () {
      if (!step('[data-vision-next]')) step('[data-vision-close]');
    }, { once: true });
    var p = vid.play();
    if (p && typeof p.catch === 'function') {
      // Autoplay refused by the browser is not an error to report: the frame is
      // fully rendered and the viewer can start it themselves.
      p.catch(function () {});
    }
  }

  // ── keyboard ───────────────────────────────────────────────────────────────

  document.addEventListener('keydown', function (e) {
    if (!viewer()) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    switch (e.key) {
      case 'ArrowRight':
      case ' ':
      case 'Spacebar':
        if (step('[data-vision-next]')) e.preventDefault();
        break;
      case 'ArrowLeft':
        if (step('[data-vision-prev]')) e.preventDefault();
        break;
      case 'Escape':
        e.preventDefault();
        if (!step('[data-vision-close]')) closeViewer();
        break;
      default:
        break;
    }
  });

  // ── touch ──────────────────────────────────────────────────────────────────

  var touchX = 0, touchY = 0, touching = false;

  document.addEventListener('touchstart', function (e) {
    if (!viewer() || !e.touches || e.touches.length !== 1) { touching = false; return; }
    touchX = e.touches[0].clientX;
    touchY = e.touches[0].clientY;
    touching = true;
  }, { passive: true });

  document.addEventListener('touchend', function (e) {
    if (!touching) return;
    touching = false;
    var t = e.changedTouches && e.changedTouches[0];
    if (!t) return;
    var dx = t.clientX - touchX;
    var dy = t.clientY - touchY;
    var ax = Math.abs(dx), ay = Math.abs(dy);
    if (ax > SWIPE_MIN && ax > ay * SWIPE_SLOPE) {
      step(dx < 0 ? '[data-vision-next]' : '[data-vision-prev]');
      return;
    }
    if (dy > SWIPE_MIN && ay > ax * SWIPE_SLOPE) {
      if (!step('[data-vision-close]')) closeViewer();
    }
  }, { passive: true });

  // ── Fragment arrival ───────────────────────────────────────────────────────

  // A close anchor is a real address so a browser running no script still lands
  // somewhere. With this file running, the overlay is torn down in place instead.
  document.addEventListener('click', function (e) {
    var close = e.target.closest && e.target.closest('[data-vision-close]');
    if (!close) return;
    e.preventDefault();
    closeViewer();
  });

  document.addEventListener('htmx:afterSwap', function (e) {
    var target = e.detail && e.detail.target;
    if (!target || target.id !== SLOT) return;
    var v = viewer();
    if (v) onFrameArrived(v);
  });

  // A frame that reached the slot with the page itself (a document load or a
  // Playground swap carrying one) is opened when the page mounts. The listeners
  // above are document-level and were installed once by this file's single
  // execution; the mount only looks at the Playground it is given.
  window.F33D3R.page('vision-viewer', function () {
    var v = viewer();
    if (v) onFrameArrived(v);
  });
})();
