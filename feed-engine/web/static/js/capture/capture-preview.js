/**
 * capture-preview.js — Capture Substrate · preview surface
 *
 * Binds a live MediaStream to the camera_preview <video> facet and manages the
 * front-camera mirror. The .camera-preview--front/--rear classes are shared with
 * the Visions and KYC cameras — do not rename them here.
 */
(function () {
  'use strict';
  window.CaptureKit = window.CaptureKit || {};

  function PreviewSurface(videoEl) {
    this.videoEl = videoEl;
  }

  PreviewSurface.prototype.bind = function (stream, facing) {
    if (!this.videoEl) return;
    if (this.videoEl.srcObject !== stream) this.videoEl.srcObject = stream;
    var isFront = facing === 'user';
    this.videoEl.classList.toggle('camera-preview--front', isFront);
    this.videoEl.classList.toggle('camera-preview--rear', !isFront);
  };

  PreviewSurface.prototype.clear = function () {
    if (!this.videoEl) return;
    try { this.videoEl.srcObject = null; } catch (e) { /* noop */ }
  };

  PreviewSurface.prototype.ready = function () {
    return !!(this.videoEl && this.videoEl.videoWidth && this.videoEl.videoHeight);
  };

  window.CaptureKit.PreviewSurface = PreviewSurface;
})();
