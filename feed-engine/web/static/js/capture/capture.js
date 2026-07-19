/**
 * capture.js — Capture Substrate · public facade
 *
 * The f33d3r camera stack. Feature-agnostic: it knows nothing about Visions,
 * react-to-post, or KYC. It opens a camera, previews it, captures a photo or
 * records a video, and hands the resulting File to whatever mounted it via the
 * registered output sink. Consuming features decide what to do with the File.
 *
 * Mount contract:
 *   window.Capture.open({ mode: 'photo'|'video', facing, onCapture: fn(File) })
 *   window.Capture.close()
 *
 * Back-compat: window.openCameraCapture() wires the works compose action bar to
 * onMediaSelect → /upload/post-media (unchanged downstream).
 */
(function () {
  'use strict';
  var K = window.CaptureKit || {};

  function Capture() {
    this.state = new K.StateMachine();
    this.registry = new K.SourceRegistry();
    this.controller = new K.Controller(this.state, this.registry);
    this.recorder = new K.Recorder();
    this.preview = null;
    this.stage = null;
    this._timerId = null;
    this._onCapture = function () {};
    this._actionsBound = false;
    this._listenersBound = false;
  }

  Capture.prototype._locate = function () {
    this.stage = document.getElementById('capture-stage');
    if (!this.stage) return false;
    var videoEl = this.stage.querySelector('[data-facet-id="facet:f33d3r:capture:preview"]');
    this.preview = new K.PreviewSurface(videoEl);
    return true;
  };

  // Project the canonical state into the dumb facets. Facets hold no state.
  Capture.prototype._render = function (s) {
    var stage = this.stage;
    if (!stage) return;

    stage.setAttribute('data-status', s.status);
    stage.setAttribute('data-mode', s.mode);
    stage.setAttribute('data-recording', s.recording ? '1' : '0');
    stage.setAttribute('data-facing', s.facing);
    stage.setAttribute('data-multicam', this.registry.hasMultipleCameras() ? '1' : '0');

    var modeBtns = stage.querySelectorAll('[data-capture-action="mode"]');
    for (var i = 0; i < modeBtns.length; i++) {
      modeBtns[i].classList.toggle('is-active', modeBtns[i].getAttribute('data-mode') === s.mode);
    }

    var micBtn = stage.querySelector('[data-capture-action="mic"]');
    if (micBtn) micBtn.classList.toggle('is-on', !!s.mic);

    var statusEl = stage.querySelector('[data-facet-id="facet:f33d3r:capture:status"]');
    if (statusEl) statusEl.textContent = this._statusLabel(s);

    var permEl = stage.querySelector('[data-facet-id="facet:f33d3r:capture:permission"]');
    if (permEl) permEl.style.display = (s.permission === 'denied') ? 'flex' : 'none';

    var timer = stage.querySelector('[data-facet-id="facet:f33d3r:capture:timer"]');
    if (timer) timer.style.display = s.recording ? 'flex' : 'none';

    var shutter = stage.querySelector('[data-capture-action="shutter"]');
    if (shutter) shutter.classList.toggle('is-recording', s.recording && s.mode === 'video');
  };

  Capture.prototype._statusLabel = function (s) {
    switch (s.status) {
      case 'requesting_permission': return 'Requesting camera…';
      case 'loading_device':        return 'Starting camera…';
      case 'error':                 return s.permission === 'denied' ? '' : 'Camera unavailable';
      default:                      return '';
    }
  };

  Capture.prototype._bindActions = function () {
    if (this._actionsBound) return;
    this._actionsBound = true;
    var self = this;
    this.stage.addEventListener('click', function (e) {
      var el = e.target.closest ? e.target.closest('[data-capture-action]') : null;
      if (!el || !self.stage.contains(el)) return;
      self._dispatch(el.getAttribute('data-capture-action'), el);
    });
  };

  Capture.prototype._bindListeners = function () {
    if (this._listenersBound) return;
    this._listenersBound = true;
    var self = this;
    this.state.on(function (s) {
      self._render(s);
      if (s.status === 'ready' && self.controller.stream) {
        self.preview.bind(self.controller.stream, s.facing);
      }
    });
  };

  Capture.prototype._dispatch = function (action, el) {
    switch (action) {
      case 'shutter': return this._shutter();
      case 'record':  return this._shutter();
      case 'stop':    return this._stopVideo();
      case 'flip':    return this.controller.flip().catch(function () {});
      case 'device':  return this.controller.cycleDevice().catch(function () {});
      case 'mic':     return this.controller.setMic(!this.state.state.mic);
      case 'torch':   return this.controller.toggleTorch();
      case 'mode':    return this.controller.setMode(el.getAttribute('data-mode')).catch(function () {});
      case 'close':   return this.close();
    }
  };

  Capture.prototype._shutter = function () {
    var s = this.state.state;
    if (s.status !== 'ready' && !this.recorder.isRecording()) return;
    if (s.mode === 'photo') {
      var self = this;
      this.state.go('processing');
      this.recorder.capturePhoto(this.preview.videoEl).then(function () { self.close(); });
    } else if (this.recorder.isRecording()) {
      this._stopVideo();
    } else {
      this._startVideo();
    }
  };

  Capture.prototype._startVideo = function () {
    if (!this.controller.stream) return;
    this.recorder.startVideo(this.controller.stream);
    this.state.go('recording', { recording: true, duration: 0 });
    var self = this;
    var started = Date.now();
    this._timerId = setInterval(function () {
      var secs = Math.floor((Date.now() - started) / 1000);
      self.state.set({ duration: secs });
      var txt = self.stage.querySelector('[data-facet-id="facet:f33d3r:capture:timer-text"]');
      if (txt) txt.textContent = Math.floor(secs / 60) + ':' + (secs % 60 < 10 ? '0' : '') + (secs % 60);
    }, 250);
  };

  Capture.prototype._stopVideo = function () {
    var self = this;
    clearInterval(this._timerId); this._timerId = null;
    this.state.go('processing', { recording: false });
    this.recorder.stopVideo().then(function () { self.close(); });
  };

  Capture.prototype.open = function (opts) {
    opts = opts || {};
    if (!this._locate()) {
      if (window.toast) window.toast('Camera unavailable', 'error');
      return;
    }
    this._onCapture = opts.onCapture || function () {};
    // Default 'record' sink hands the finalized File to the mount's callback.
    this.recorder.registerSink('record', this._onCapture).useSink('record');

    this._bindListeners();
    this._bindActions();

    this.stage.style.display = 'flex';
    var self = this;
    this.controller.open({ mode: opts.mode || 'photo', facing: opts.facing || 'user' })
      .then(function (stream) { if (stream) self.preview.bind(stream, self.state.state.facing); })
      .catch(function () { /* state already in error; _render shows the message */ });
  };

  Capture.prototype.close = function () {
    clearInterval(this._timerId); this._timerId = null;
    this.recorder.abort();
    this.controller.stop();
    if (this.preview) this.preview.clear();
    if (this.stage) this.stage.style.display = 'none';
    this.state.go('idle', { recording: false, duration: 0 });
  };

  var instance = new Capture();
  window.Capture = instance;

  // Back-compat entry used by the works compose action bar (_compose_modal.html).
  // Output handoff is unchanged: a File flows into onMediaSelect → /upload/post-media.
  window.openCameraCapture = function () {
    if (typeof window.onMediaSelect !== 'function') {
      if (window.toast) window.toast('Compose not ready', 'error');
      return;
    }
    instance.open({
      mode: 'photo',
      onCapture: function (file) { window.onMediaSelect({ files: [file] }); }
    });
  };
})();
