/**
 * capture-recorder.js — Capture Substrate · recorder + output sinks
 *
 * Captures still frames (canvas grab) and video (MediaRecorder), true-to-frame
 * with no effects — feature-level looks (LUTs, vignette) live in the consuming
 * feature, never here. Output is delivered to a registered sink. The 'record'
 * sink hands a finalized File to the mount's callback; a future 'live' sink can
 * be registered without touching capture logic.
 */
(function () {
  'use strict';
  window.CaptureKit = window.CaptureKit || {};

  function Recorder() {
    this._mr = null;
    this._chunks = [];
    this._sinks = {};          // name → fn(file)
    this._activeSink = 'record';
    this._mime = '';
  }

  Recorder.prototype.registerSink = function (name, fn) {
    this._sinks[name] = fn;
    return this;
  };

  Recorder.prototype.useSink = function (name) {
    this._activeSink = name;
    return this;
  };

  Recorder.prototype._deliver = function (file) {
    var sink = this._sinks[this._activeSink];
    if (sink) sink(file);
    else console.warn('[capture] no sink registered: ' + this._activeSink);
  };

  // Photo: grab the current preview frame, true-to-frame.
  Recorder.prototype.capturePhoto = function (videoEl) {
    var w = videoEl.videoWidth || 1280;
    var h = videoEl.videoHeight || 720;
    var canvas = document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    canvas.getContext('2d').drawImage(videoEl, 0, 0, w, h);
    var self = this;
    return new Promise(function (resolve) {
      canvas.toBlob(function (blob) {
        var file = new File([blob], 'capture.jpg', { type: 'image/jpeg' });
        self._deliver(file);
        resolve(file);
      }, 'image/jpeg', 0.92);
    });
  };

  Recorder.prototype.isRecording = function () {
    return !!(this._mr && this._mr.state === 'recording');
  };

  Recorder.prototype.startVideo = function (stream, opts) {
    // opts.alpha → prefer VP8, the codec that reliably preserves a transparent
    // background (used by React-With-Video green screen).
    var order = (opts && opts.alpha)
      ? ['video/webm;codecs=vp8,opus', 'video/webm;codecs=vp8', 'video/webm']
      : ['video/webm;codecs=vp9,opus', 'video/webm;codecs=vp8,opus', 'video/webm', 'video/mp4'];
    this._mime = order.find(function (m) { return window.MediaRecorder && MediaRecorder.isTypeSupported(m); }) || '';
    this._chunks = [];
    this._mr = this._mime ? new MediaRecorder(stream, { mimeType: this._mime })
                          : new MediaRecorder(stream);
    var self = this;
    this._mr.ondataavailable = function (e) { if (e.data && e.data.size > 0) self._chunks.push(e.data); };
    this._mr.start();
  };

  // Stop and flush; resolves with the delivered File.
  Recorder.prototype.stopVideo = function () {
    var self = this;
    return new Promise(function (resolve) {
      if (!self._mr) { resolve(null); return; }
      self._mr.onstop = function () {
        var mime = self._mr.mimeType || self._mime || 'video/webm';
        var ext = mime.indexOf('mp4') !== -1 ? 'mp4' : 'webm';
        var blob = new Blob(self._chunks, { type: mime });
        var file = new File([blob], 'capture.' + ext, { type: mime });
        self._mr = null;
        self._chunks = [];
        self._deliver(file);
        resolve(file);
      };
      self._mr.stop();
    });
  };

  // Tear down a recording without delivering anything (used on close).
  Recorder.prototype.abort = function () {
    if (this._mr) {
      this._mr.onstop = null;
      if (this._mr.state === 'recording') { try { this._mr.stop(); } catch (e) { /* already stopped */ } }
    }
    this._mr = null;
    this._chunks = [];
  };

  window.CaptureKit.Recorder = Recorder;
})();
