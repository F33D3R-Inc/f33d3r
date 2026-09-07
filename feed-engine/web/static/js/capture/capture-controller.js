/**
 * capture-controller.js — Capture Substrate · stream lifecycle
 *
 * Owns the MediaStream: open/close, device switch, facing flip, mic toggle, mode
 * change, and advanced constraints (continuous focus/exposure, torch) when the
 * device supports them. Drives the state machine. No UI, no DOM.
 */
(function () {
  'use strict';
  window.CaptureKit = window.CaptureKit || {};

  function Controller(state, registry) {
    this.state = state;
    this.registry = registry;
    this.stream = null;
    this.videoTrack = null;
    this.torchSupported = false;
    this.torchOn = false;
  }

  // Open a stream for the requested facing/mode/device. Walks the state machine
  // requesting_permission → loading_device → ready (or → error).
  Controller.prototype.open = function (opts) {
    opts = opts || {};
    var facing = opts.facing || this.state.state.facing || 'user';
    var mode = opts.mode || this.state.state.mode || 'photo';
    var deviceId = opts.deviceId || null;
    var self = this;

    this.stop();

    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      this.state.go('error', { error: 'unsupported', permission: 'unknown' });
      return Promise.reject(new Error('getUserMedia unsupported'));
    }

    this.state.go('requesting_permission', { facing: facing, mode: mode, permission: 'prompt' });

    var video = deviceId ? { deviceId: { exact: deviceId } } : { facingMode: facing };
    video.width = { ideal: 1920 };
    video.height = { ideal: 1080 };

    return navigator.mediaDevices.getUserMedia({ video: video, audio: mode === 'video' })
      .then(function (stream) {
        self.stream = stream;
        self.videoTrack = stream.getVideoTracks()[0] || null;
        self.state.go('loading_device', { permission: 'granted', mic: mode === 'video' });
        self._applyAdvanced();
        // Labels unlock once permission is granted — refresh, then settle to ready.
        return self.registry.refresh().then(function () {
          if (self.videoTrack && self.videoTrack.getSettings) {
            var sid = self.videoTrack.getSettings().deviceId;
            if (sid) self.registry.remember(sid);
          }
          self.state.go('ready', { source: self.registry.activeVideoId });
          return stream;
        });
      })
      .catch(function (err) {
        console.error('[capture] getUserMedia failed', err);
        var denied = err && (err.name === 'NotAllowedError' || err.name === 'SecurityError');
        self.state.go('error', {
          error: (err && err.name) ? err.name : 'error',
          permission: denied ? 'denied' : self.state.state.permission
        });
        if (denied && window.Permissions && typeof window.Permissions.showDeniedHelp === 'function') {
          window.Permissions.showDeniedHelp('camera');
        }
        throw err;
      });
  };

  Controller.prototype._applyAdvanced = function () {
    var track = this.videoTrack;
    this.torchSupported = false;
    this.torchOn = false;
    if (!track || !track.getCapabilities) return;
    try {
      var caps = track.getCapabilities();
      var adv = [];
      if (caps.focusMode && caps.focusMode.indexOf('continuous') !== -1) adv.push({ focusMode: 'continuous' });
      if (caps.exposureMode && caps.exposureMode.indexOf('continuous') !== -1) adv.push({ exposureMode: 'continuous' });
      if (adv.length) track.applyConstraints({ advanced: adv });
      this.torchSupported = caps.torch === true;
    } catch (e) { /* advanced constraints unsupported — harmless */ }
  };

  Controller.prototype.flip = function () {
    var next = (this.state.state.facing === 'user') ? 'environment' : 'user';
    return this.open({ facing: next, mode: this.state.state.mode });
  };

  Controller.prototype.cycleDevice = function () {
    var id = this.registry.nextVideoId();
    if (!id) return Promise.resolve();
    return this.open({ deviceId: id, mode: this.state.state.mode });
  };

  Controller.prototype.setMode = function (mode) {
    if (mode === this.state.state.mode) return Promise.resolve();
    // Reopen so the audio track is added/removed to match the mode.
    return this.open({ facing: this.state.state.facing, mode: mode });
  };

  Controller.prototype.setMic = function (on) {
    if (!this.stream) return;
    this.stream.getAudioTracks().forEach(function (t) { t.enabled = on; });
    this.state.set({ mic: on });
  };

  Controller.prototype.toggleTorch = function () {
    if (!this.videoTrack || !this.torchSupported) return;
    this.torchOn = !this.torchOn;
    var self = this;
    try { this.videoTrack.applyConstraints({ advanced: [{ torch: this.torchOn }] }); }
    catch (e) { self.torchOn = !self.torchOn; }
  };

  Controller.prototype.stop = function () {
    if (this.stream) {
      this.stream.getTracks().forEach(function (t) { t.stop(); });
      this.stream = null;
      this.videoTrack = null;
    }
    this.torchSupported = false;
    this.torchOn = false;
  };

  window.CaptureKit.Controller = Controller;
})();
