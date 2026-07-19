/**
 * capture-devices.js — Capture Substrate · source registry
 *
 * Device discovery and selection. The UI never touches hardware — it only ever
 * reads registry entries. Labels are only exposed by the browser after camera
 * permission is granted, so refresh() should be called once a stream is live.
 */
(function () {
  'use strict';
  window.CaptureKit = window.CaptureKit || {};

  var REMEMBER_KEY = 'f33d3r.capture.deviceId';

  function SourceRegistry() {
    this.video = [];          // [{ id, label }]
    this.audio = [];          // [{ id, label }]
    this.activeVideoId = null;
  }

  SourceRegistry.prototype.refresh = function () {
    if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) {
      return Promise.resolve(this);
    }
    var self = this;
    return navigator.mediaDevices.enumerateDevices().then(function (devices) {
      self.video = [];
      self.audio = [];
      devices.forEach(function (d) {
        if (d.kind === 'videoinput') {
          self.video.push({ id: d.deviceId, label: d.label || ('Camera ' + (self.video.length + 1)) });
        } else if (d.kind === 'audioinput') {
          self.audio.push({ id: d.deviceId, label: d.label || ('Mic ' + (self.audio.length + 1)) });
        }
      });
      return self;
    }).catch(function () { return self; });
  };

  SourceRegistry.prototype.hasMultipleCameras = function () {
    return this.video.length > 1;
  };

  SourceRegistry.prototype.remember = function (id) {
    this.activeVideoId = id || null;
    try { localStorage.setItem(REMEMBER_KEY, id || ''); } catch (e) { /* private mode */ }
  };

  SourceRegistry.prototype.recalled = function () {
    try { return localStorage.getItem(REMEMBER_KEY) || null; } catch (e) { return null; }
  };

  // The next camera after the active one, for device cycling.
  SourceRegistry.prototype.nextVideoId = function () {
    if (this.video.length < 2) return this.activeVideoId;
    var idx = -1;
    for (var i = 0; i < this.video.length; i++) {
      if (this.video[i].id === this.activeVideoId) { idx = i; break; }
    }
    return this.video[(idx + 1) % this.video.length].id;
  };

  window.CaptureKit.SourceRegistry = SourceRegistry;
})();
