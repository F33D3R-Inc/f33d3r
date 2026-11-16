// creator_dashboard.js — Creator Dashboard nav sync + upload flow
// Registered as a Shell page: executed once per session, the mount runs on
// every arrival of the dashboard Playground.
'use strict';

F33D3R.page('creator_dashboard', function (root) {
  // ── Document-level listeners — installed once ───────────────────────────────
  F33D3R.once('creator_dashboard:listeners', function () {
    // Sync nav highlight on any HTMX request into #creator-panel
    document.body.addEventListener('htmx:beforeRequest', function(e) {
      var target = e.detail && e.detail.target;
      if (!target || target.id !== 'creator-panel') return;
      var path = (e.detail.requestConfig && e.detail.requestConfig.path) || '';
      document.querySelectorAll('#creator-nav-panel .settings-nav-item').forEach(function(item) {
        var hxGet = item.getAttribute('hx-get') || '';
        var seg = hxGet.replace('/create/partials/', '').split('?')[0];
        item.classList.toggle('active', path.indexOf(seg) !== -1 && seg !== '');
      });
    });
    // Drop zone wiring after every HTMX swap into #creator-panel
    document.addEventListener('htmx:afterSwap', function(e) {
      if (e.target && e.target.id === 'creator-panel' && window.crtWireDropZone) window.crtWireDropZone();
    });
  });

  function crtWireDropZone() {
    var zone   = document.getElementById('crt-drop-zone');
    var input  = document.getElementById('crt-audio-input');
    var btn    = document.getElementById('crt-upload-btn');
    var cancel = document.getElementById('crt-upload-cancel');
    if (!zone || zone.dataset.wired) return;
    zone.dataset.wired = '1';

    zone.addEventListener('click', function() { if (input) input.click(); });
    zone.addEventListener('keydown', function(e) {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); if (input) input.click(); }
    });
    zone.addEventListener('dragover', function(e) {
      e.preventDefault();
      zone.classList.add('stu-drop-zone--drag');
    });
    zone.addEventListener('dragleave', function() {
      zone.classList.remove('stu-drop-zone--drag');
    });
    zone.addEventListener('drop', function(e) {
      e.preventDefault();
      zone.classList.remove('stu-drop-zone--drag');
      var file = e.dataTransfer && e.dataTransfer.files[0];
      if (file) crtShowUploadForm(file);
    });
    if (input) {
      input.addEventListener('change', function() {
        if (input.files && input.files[0]) crtShowUploadForm(input.files[0]);
      });
    }
    if (btn)    btn.addEventListener('click', crtSubmitUpload);
    if (cancel) cancel.addEventListener('click', crtCancelUpload);
  }

  // ── Track upload flow ──────────────────────────────────────────────────────────
  var _crtSelectedFile = null;

  function crtShowUploadForm(file) {
    _crtSelectedFile = file;
    var zone    = document.getElementById('crt-drop-zone');
    var wrap    = document.getElementById('crt-upload-form-wrap');
    var preview = document.getElementById('crt-file-preview');
    if (zone)    zone.style.display = 'none';
    if (wrap)    wrap.style.display = '';
    if (preview) preview.textContent = file.name + ' (' + (file.size / (1024 * 1024)).toFixed(1) + ' MB)';
  }

  function crtCancelUpload() {
    _crtSelectedFile = null;
    var zone = document.getElementById('crt-drop-zone');
    var wrap = document.getElementById('crt-upload-form-wrap');
    if (zone) zone.style.display = '';
    if (wrap) wrap.style.display = 'none';
    var form = document.getElementById('crt-upload-form');
    if (form) form.reset();
  }

  function crtSubmitUpload() {
    var form = document.getElementById('crt-upload-form');
    var btn  = document.getElementById('crt-upload-btn');
    var bar  = document.getElementById('crt-progress-bar');
    var stat = document.getElementById('crt-progress-status');
    var prog = document.getElementById('crt-upload-progress');

    if (!form) return;
    if (!form.checkValidity()) { form.reportValidity(); return; }

    var fd = new FormData(form);
    if (_crtSelectedFile) fd.set('audio', _crtSelectedFile);

    if (btn)  { btn.disabled = true; btn.textContent = 'Uploading…'; }
    if (prog)   prog.style.display = 'flex';

    var xhr = new XMLHttpRequest();
    xhr.open('POST', '/upload/track');
    xhr.upload.addEventListener('progress', function(e) {
      if (e.lengthComputable && bar && stat) {
        var pct = Math.round(e.loaded / e.total * 100);
        bar.style.width = pct + '%';
        stat.textContent = pct + '%';
      }
    });
    // The upload stores the audio and cover; the track becomes a work when the
    // artist's key signs it and the server creates it. The outcome arrives as
    // the compose status Facet over FA Live.
    xhr.onload = function() {
      if (xhr.status !== 200) {
        if (stat) stat.textContent = 'Upload failed';
        if (typeof toast === 'function') toast(xhr.responseText || 'Upload failed', 'error');
        if (btn) { btn.disabled = false; btn.textContent = 'Upload track'; }
        return;
      }
      var stored;
      try { stored = JSON.parse(xhr.responseText); } catch (e) { stored = null; }
      if (!stored || !stored.audio_url) {
        if (stat) stat.textContent = 'Upload failed';
        if (btn) { btn.disabled = false; btn.textContent = 'Upload track'; }
        return;
      }
      if (stat) stat.textContent = 'Signing…';
      crtPublishTrack(form, stored).then(function(ok) {
        if (ok) {
          if (stat) stat.textContent = '✓ Done';
          if (bar)  bar.style.width  = '100%';
          crtCancelUpload();
          setTimeout(function() {
            if (prog) prog.style.display = 'none';
            if (bar)  bar.style.width    = '0';
            htmx.ajax('GET', '/create/partials/music', { target: '#creator-panel', swap: 'innerHTML' });
          }, 800);
        } else if (stat) {
          stat.textContent = 'Publishing failed';
        }
        if (btn) { btn.disabled = false; btn.textContent = 'Upload track'; }
      });
    };
    xhr.onerror = function() {
      if (stat) stat.textContent = 'Network error';
      if (btn)  { btn.disabled = false; btn.textContent = 'Upload track'; }
    };
    xhr.send(fd);
  }

  // crtTrackDuration reads the track's length from the file the artist chose,
  // the way the voice recorder reports its own: the signer states it.
  function crtTrackDuration(file) {
    return new Promise(function(resolve) {
      if (!file) { resolve(null); return; }
      var url = URL.createObjectURL(file);
      var a = new Audio();
      a.preload = 'metadata';
      a.onloadedmetadata = function() { URL.revokeObjectURL(url); resolve(isFinite(a.duration) && a.duration > 0 ? a.duration : null); };
      a.onerror = function() { URL.revokeObjectURL(url); resolve(null); };
      a.src = url;
    });
  }

  // crtPublishTrack signs the track as a work of kind 'track' and posts it.
  async function crtPublishTrack(form, stored) {
    if (!window.Malkuth || !window._malkuthReady) {
      if (typeof toast === 'function') toast('Signing key unavailable — please reload the page.', 'error');
      return false;
    }
    var mk = await window._malkuthReady;
    if (!mk || !mk.hasKey) {
      if (typeof toast === 'function') toast('Signing key unavailable — please reload the page.', 'error');
      return false;
    }
    var title = (form.querySelector('[name="title"]') || {}).value || '';
    var desc  = (form.querySelector('[name="description"]') || {}).value || '';
    var genre = ((form.querySelector('[name="genre"]') || {}).value || '').replace(/\s+/g, '');
    var body  = title.trim() || 'Untitled';
    if (desc.trim()) body += '\n\n' + desc.trim();
    var duration = await crtTrackDuration(_crtSelectedFile);
    var env = await window.Malkuth.buildSignedWork('post', body, 'track',
      stored.cover_url ? [stored.cover_url] : [], null, {
        voice_url: stored.audio_url,
        voice_duration_secs: duration,
        tags: genre ? [genre] : []
      });
    var r = await fetch('/events', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'HX-Request': 'true' },
      body: JSON.stringify(env)
    });
    return r.ok;
  }

  // Track delete is a hypermedia control on the row itself (hx-post
  // /api/track/delete, hx-swap="delete") — no page script.

  window.crtWireDropZone  = crtWireDropZone;
  window.crtShowUploadForm = crtShowUploadForm;
  window.crtCancelUpload  = crtCancelUpload;
  window.crtSubmitUpload  = crtSubmitUpload;

  crtWireDropZone();
});
