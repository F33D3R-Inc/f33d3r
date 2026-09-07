/**
 * react-video.js — React With Video · studio orchestrator
 *
 * First feature to consume the camera substrate (CaptureKit.*) directly — it
 * does NOT use the compose-camera facade. Boots the substrate into the
 * server-rendered studio overlay (which embeds the original via the quote
 * template), plays the original, applies a layout, records the camera reaction,
 * and posts it as kind='react_video' quoting the original. The original never
 * gets baked into the reaction — it rides along as a quote, arranged at view
 * time by react_layout. Green screen records an alpha clip via RVLayouts.
 */
(function () {
  'use strict';
  var K = window.CaptureKit || {};

  function RV() {
    this.root = null;
    this.workId = null;
    this.layout = 'presenter';
    this.timerId = null;
    this.recording = false;
  }

  RV.prototype.q = function (facetID) {
    return this.root.querySelector('[data-facet-id="' + facetID + '"]');
  };

  RV.prototype.boot = function (root) {
    this.root = root;
    this.workId = root.getAttribute('data-work-id');
    this.layout = root.getAttribute('data-rv-layout') || 'presenter';

    this.state = new K.StateMachine();
    this.registry = new K.SourceRegistry();
    this.controller = new K.Controller(this.state, this.registry);
    this.recorder = new K.Recorder();
    this.preview = new K.PreviewSurface(this.q('facet:f33d3r:capture:preview'));

    var self = this;
    this.state.on(function (s) {
      self.render(s);
      if (s.status === 'ready' && self.controller.stream) {
        self.preview.bind(self.controller.stream, s.facing);
      }
    });

    root.addEventListener('click', function (e) {
      var el = e.target.closest ? e.target.closest('[data-rv-action]') : null;
      if (!el || !root.contains(el)) return;
      self.dispatch(el.getAttribute('data-rv-action'), el);
    });

    // Reaction is always video → open with mic.
    this.controller.open({ mode: 'video', facing: 'user' })
      .then(function (stream) { if (stream) self.preview.bind(stream, self.state.state.facing); })
      .catch(function () { /* state already in error; render shows it */ });
  };

  RV.prototype.render = function (s) {
    var root = this.root;
    root.setAttribute('data-status', s.status);
    root.setAttribute('data-recording', s.recording ? '1' : '0');
    var statusEl = this.q('facet:f33d3r:reactvideo:status');
    if (statusEl) statusEl.textContent = this.statusLabel(s);
    var permEl = this.q('facet:f33d3r:reactvideo:permission');
    if (permEl) permEl.style.display = (s.permission === 'denied') ? 'flex' : 'none';
    var timer = this.q('facet:f33d3r:reactvideo:timer');
    if (timer) timer.style.display = s.recording ? 'flex' : 'none';
  };

  RV.prototype.statusLabel = function (s) {
    if (s.status === 'requesting_permission') return 'Requesting camera…';
    if (s.status === 'loading_device') return 'Starting camera…';
    if (s.status === 'error') return s.permission === 'denied' ? '' : 'Camera unavailable';
    return '';
  };

  RV.prototype.dispatch = function (action, el) {
    switch (action) {
      case 'close':  return this.close();
      case 'flip':   return this.controller.flip().catch(function () {});
      case 'layout': return this.setLayout(el.getAttribute('data-layout'), el);
      case 'record': return this.toggleRecord();
    }
  };

  RV.prototype.setLayout = function (layout, btn) {
    if (!layout) return;
    this.layout = layout;
    this.root.setAttribute('data-rv-layout', layout);

    var btns = this.root.querySelectorAll('[data-rv-action="layout"]');
    for (var i = 0; i < btns.length; i++) btns[i].classList.toggle('is-active', btns[i] === btn);

    var label = this.q('facet:f33d3r:reactvideo:layout-label');
    if (label) {
      label.textContent = btn.getAttribute('title') || layout;
      label.classList.add('show');
      clearTimeout(this._lblT);
      this._lblT = setTimeout(function () { label.classList.remove('show'); }, 1400);
    }

    var canvas = this.q('facet:f33d3r:reactvideo:canvas');
    if (layout === 'green_screen' && window.RVLayouts) {
      window.RVLayouts.greenScreen.start(this.preview.videoEl, canvas);
    } else if (window.RVLayouts) {
      window.RVLayouts.greenScreen.stop();
    }
  };

  RV.prototype.toggleRecord = function () {
    if (this.recording) this.stopRecord();
    else this.startRecord();
  };

  RV.prototype.startRecord = function () {
    var stream;
    var alpha = false;
    if (this.layout === 'green_screen' && window.RVLayouts) {
      var canvasStream = window.RVLayouts.greenScreen.getCanvasStream();
      if (canvasStream) {
        // Composite the keyed canvas video with the camera's audio track.
        stream = new MediaStream();
        canvasStream.getVideoTracks().forEach(function (t) { stream.addTrack(t); });
        if (this.controller.stream) {
          this.controller.stream.getAudioTracks().forEach(function (t) { stream.addTrack(t); });
        }
        alpha = true;
      }
    }
    if (!stream) stream = this.controller.stream;
    if (!stream) return;

    this.recorder.startVideo(stream, { alpha: alpha });
    this.recording = true;
    this.state.go('recording', { recording: true, duration: 0 });

    var self = this;
    var started = Date.now();
    this.timerId = setInterval(function () {
      var secs = Math.floor((Date.now() - started) / 1000);
      var txt = self.q('facet:f33d3r:reactvideo:timer-text');
      if (txt) txt.textContent = Math.floor(secs / 60) + ':' + (secs % 60 < 10 ? '0' : '') + (secs % 60);
    }, 250);
  };

  RV.prototype.stopRecord = function () {
    var self = this;
    clearInterval(this.timerId); this.timerId = null;
    this.recording = false;
    this.state.go('processing', { recording: false });
    this.recorder.stopVideo().then(function (file) {
      if (file) self.publish(file);
      else self.state.go('ready');
    });
  };

  RV.prototype.publish = function (file) {
    var self = this;
    var sending = this.q('facet:f33d3r:reactvideo:sending');
    if (sending) sending.style.display = 'flex';

    var capEl = this.q('facet:f33d3r:reactvideo:caption');
    var caption = capEl ? capEl.value.trim() : '';

    var fd = new FormData();
    fd.append('video', file, 'reaction.webm');

    fetch('/upload/react-video', { method: 'POST', body: fd })
      .then(function (r) { if (!r.ok) throw new Error('upload ' + r.status); return r.json(); })
      .then(function (d) {
        if (!d.url) throw new Error('no url');
        if (!window.Malkuth || !window._malkuthReady) throw new Error('Malkuth not loaded');
        return window._malkuthReady.then(function (mk) {
          if (!mk || !mk.hasKey) throw new Error('signing key unavailable');
          return window.Malkuth.buildSignedWork('post', caption, 'react_video', [], null, {
            quoted_work_id:   self.workId,
            video_master_url: d.url,
            react_layout:     self.layout
          });
        });
      })
      .then(function (env) {
        return fetch('/events', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'HX-Request': 'true' },
          body: JSON.stringify(env)
        });
      })
      .then(function (r) {
        if (!r.ok) throw new Error('post ' + r.status);
        if (window.toast) window.toast('Reaction posted');
        self.close();
        if (window.F33D3R && window.F33D3R.visit) window.F33D3R.visit('/'); else window.location.href = '/';
      })
      .catch(function (err) {
        console.error('[react-video] publish failed:', err);
        if (sending) sending.style.display = 'none';
        self.state.go('ready');
        if (window.toast) window.toast('Could not post reaction. Please try again.', 'error');
      });
  };

  RV.prototype.close = function () {
    clearInterval(this.timerId); this.timerId = null;
    if (window.RVLayouts) { try { window.RVLayouts.greenScreen.stop(); } catch (e) {} }
    if (this.recorder) this.recorder.abort();
    if (this.controller) this.controller.stop();
    if (this.root) this.root.remove();
    if (window.__rvStudio === this) window.__rvStudio = null;
  };

  // Entry — wired from the work repost/quote sheet ("React with Video").
  window.openReactWithVideo = function (workId /*, authorHandle */) {
    if (!workId) return;
    fetch('/react-video/' + encodeURIComponent(workId), { headers: { 'X-FA-Navigate': 'true' } })
      .then(function (r) { if (!r.ok) throw new Error('load ' + r.status); return r.text(); })
      .then(function (html) {
        var old = document.getElementById('rv-studio');
        if (old) old.remove();
        var tmp = document.createElement('div');
        tmp.innerHTML = html.trim();
        var root = tmp.firstElementChild;
        if (!root) throw new Error('empty studio');
        document.body.appendChild(root);
        var rv = new RV();
        window.__rvStudio = rv;
        rv.boot(root);
      })
      .catch(function (err) {
        console.error('[react-video] open failed:', err);
        if (window.toast) window.toast('Could not open React with Video', 'error');
      });
  };
})();
