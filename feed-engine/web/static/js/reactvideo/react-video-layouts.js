/**
 * react-video-layouts.js — React With Video · green-screen compositor
 *
 * Real-time background removal for the green-screen layout. Pulls the live
 * camera frames, runs MediaPipe Selfie Segmentation, and draws the keyed
 * reactor onto a transparent canvas. The canvas captureStream is recorded as
 * alpha WebM so the quoted post shows through at view time.
 *
 * The segmentation lib is loaded on demand (browser fetches it, like hls.min.js
 * is lazy-loaded). If it is unavailable, we fall back to drawing the raw frame
 * (opaque) so recording still produces a usable clip — keying simply doesn't
 * activate until the lib is reachable / self-hosted.
 */
(function () {
  'use strict';

  var MP_BASE = 'https://cdn.jsdelivr.net/npm/@mediapipe/selfie_segmentation/';
  var libPromise = null;

  function loadLib() {
    if (window.SelfieSegmentation) return Promise.resolve(true);
    if (libPromise) return libPromise;
    libPromise = new Promise(function (resolve) {
      var s = document.createElement('script');
      s.src = MP_BASE + 'selfie_segmentation.js';
      s.async = true;
      s.crossOrigin = 'anonymous';
      s.onload = function () { resolve(!!window.SelfieSegmentation); };
      s.onerror = function () { resolve(false); };
      document.head.appendChild(s);
    });
    return libPromise;
  }

  var GreenScreen = {
    _seg: null, _canvas: null, _ctx: null, _video: null,
    _raf: null, _running: false, _ready: false,

    start: function (videoEl, canvasEl) {
      if (this._running) return;
      this._running = true;
      this._video = videoEl;
      this._canvas = canvasEl;
      this._ctx = canvasEl.getContext('2d');
      var self = this;
      loadLib().then(function (ok) {
        if (!self._running) return;
        if (ok && window.SelfieSegmentation) {
          self._seg = new window.SelfieSegmentation({ locateFile: function (f) { return MP_BASE + f; } });
          self._seg.setOptions({ modelSelection: 1, selfieMode: true });
          self._seg.onResults(function (res) { self._drawKeyed(res); });
          self._ready = true;
        }
        self._loop();
      });
    },

    _loop: function () {
      var self = this;
      var run = function () {
        if (!self._running) return;
        var v = self._video;
        if (v && v.videoWidth) {
          self._canvas.width = v.videoWidth;
          self._canvas.height = v.videoHeight;
          if (self._ready && self._seg) {
            self._seg.send({ image: v })
              .then(function () { self._raf = requestAnimationFrame(run); })
              .catch(function () { self._drawRaw(); self._raf = requestAnimationFrame(run); });
            return;
          }
          self._drawRaw();
        }
        self._raf = requestAnimationFrame(run);
      };
      run();
    },

    // Keep the reactor (mask), drop everything else → transparent background.
    _drawKeyed: function (res) {
      var ctx = this._ctx, c = this._canvas;
      ctx.save();
      ctx.clearRect(0, 0, c.width, c.height);
      ctx.drawImage(res.segmentationMask, 0, 0, c.width, c.height);
      ctx.globalCompositeOperation = 'source-in';
      ctx.drawImage(res.image, 0, 0, c.width, c.height);
      ctx.restore();
    },

    // Fallback when segmentation isn't available — opaque raw frame.
    _drawRaw: function () {
      var ctx = this._ctx, c = this._canvas;
      ctx.clearRect(0, 0, c.width, c.height);
      ctx.drawImage(this._video, 0, 0, c.width, c.height);
    },

    getCanvasStream: function () {
      return this._canvas ? this._canvas.captureStream(30) : null;
    },

    stop: function () {
      this._running = false;
      this._ready = false;
      if (this._raf) { cancelAnimationFrame(this._raf); this._raf = null; }
      if (this._seg) { try { this._seg.close(); } catch (e) { /* noop */ } this._seg = null; }
    }
  };

  window.RVLayouts = { greenScreen: GreenScreen };
})();
