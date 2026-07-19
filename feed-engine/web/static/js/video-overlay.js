/* video-overlay.js — countdown timer + creator pill wiring.
 * READ-ONLY listeners only (timeupdate, durationchange, loadedmetadata).
 * Does NOT add IntersectionObserver — video-player.js owns pause/play/reset.
 * Facet(video_bottom_pills) / Facet(video_timer_pill)
 */
(function () {
  function fmtSecs(s) {
    if (!isFinite(s) || s < 0) s = 0;
    s = Math.ceil(s);
    var m = Math.floor(s / 60);
    var sec = s % 60;
    return m + ':' + (sec < 10 ? '0' : '') + sec;
  }

  function wireOverlays() {
    document.querySelectorAll('.vp-wrap:not([data-overlay-wired])').forEach(function (wrap) {
      wrap.dataset.overlayWired = '1';
      var video = wrap.querySelector('video.post-media-video');
      var timerEl = wrap.querySelector('.video-timer-pill');
      if (!video || !timerEl) return;

      function updateTimer() {
        var dur = isFinite(video.duration) ? video.duration : 0;
        var remaining = dur - (video.currentTime || 0);
        timerEl.textContent = fmtSecs(remaining);
      }

      video.addEventListener('loadedmetadata', updateTimer);
      video.addEventListener('durationchange', updateTimer);
      video.addEventListener('timeupdate', updateTimer);
    });
  }

  document.addEventListener('DOMContentLoaded', wireOverlays);
  document.addEventListener('htmx:afterSwap', wireOverlays);
})();
