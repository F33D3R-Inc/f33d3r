/**
 * capture-state.js — Capture Substrate · state machine
 *
 * The single source of truth for the camera stack. Facets never own state;
 * they read snapshots emitted here. Treated as a hardware substrate (same
 * status FA grants the Malkuth Substrate), not application state.
 *
 * Lifecycle:
 *   idle → requesting_permission → loading_device → ready
 *   ready ⇄ recording → processing → ready
 *   (any) → error → idle
 */
(function () {
  'use strict';
  window.CaptureKit = window.CaptureKit || {};

  // Allowed status transitions. Anything not listed is rejected and logged.
  var TRANSITIONS = {
    idle:                  ['requesting_permission', 'error'],
    requesting_permission: ['loading_device', 'error', 'idle'],
    loading_device:        ['ready', 'error', 'idle'],
    ready:                 ['recording', 'processing', 'loading_device', 'requesting_permission', 'idle', 'error'],
    recording:             ['processing', 'ready', 'error', 'idle'],
    processing:            ['ready', 'idle', 'error'],
    error:                 ['idle', 'requesting_permission']
  };

  function StateMachine() {
    this._listeners = [];
    this.state = {
      status:     'idle',     // one of the TRANSITIONS keys
      source:     null,       // active video device id
      facing:     'user',     // 'user' | 'environment'
      mode:       'photo',    // 'photo' | 'video'
      mic:        false,      // audio track enabled
      recording:  false,
      permission: 'unknown',  // 'granted' | 'denied' | 'prompt' | 'unknown'
      duration:   0,          // recording seconds
      error:      null        // last error name
    };
  }

  StateMachine.prototype.on = function (fn) {
    this._listeners.push(fn);
    return this;
  };

  // Shallow copy so facets can never mutate the canonical state object.
  StateMachine.prototype.snapshot = function () {
    var s = this.state, out = {};
    for (var k in s) { if (Object.prototype.hasOwnProperty.call(s, k)) out[k] = s[k]; }
    return out;
  };

  StateMachine.prototype._emit = function () {
    var snap = this.snapshot();
    for (var i = 0; i < this._listeners.length; i++) {
      try { this._listeners[i](snap); } catch (e) { console.error('[capture] listener error', e); }
    }
  };

  StateMachine.prototype._patch = function (patch) {
    if (patch) {
      for (var k in patch) { if (Object.prototype.hasOwnProperty.call(patch, k)) this.state[k] = patch[k]; }
    }
    this._emit();
  };

  // Guarded status transition (optionally patches other fields). Returns true if allowed.
  StateMachine.prototype.go = function (next, patch) {
    var cur = this.state.status;
    if (next !== cur && (TRANSITIONS[cur] || []).indexOf(next) === -1) {
      console.warn('[capture] illegal transition ' + cur + ' → ' + next);
      return false;
    }
    this.state.status = next;
    this._patch(patch);
    return true;
  };

  // Field update with no status change (duration tick, mic toggle, …).
  StateMachine.prototype.set = function (patch) {
    this._patch(patch);
  };

  window.CaptureKit.StateMachine = StateMachine;
})();
