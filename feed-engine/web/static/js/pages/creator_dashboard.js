// creator_dashboard.js — Creator Dashboard nav sync + upload flow
// v=20260516f
'use strict';

// ── Sync nav highlight on any HTMX request into #creator-panel ────────────────
(function() {
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
})();

// ── Drop zone wiring (runs after every HTMX swap into #creator-panel) ──────────
document.addEventListener('htmx:afterSwap', function(e) {
  if (e.target && e.target.id === 'creator-panel') crtWireDropZone();
});
document.addEventListener('DOMContentLoaded', crtWireDropZone);

function crtWireDropZone() {
  var zone   = document.getElementById('crt-drop-zone');
  var input  = document.getElementById('crt-audio-input');
  var btn    = document.getElementById('crt-upload-btn');
  var cancel = document.getElementById('crt-upload-cancel');
  if (!zone) return;

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
  xhr.onload = function() {
    if (xhr.status === 200) {
      if (stat) stat.textContent = '✓ Done';
      if (bar)  bar.style.width  = '100%';
      crtCancelUpload();
      setTimeout(function() {
        if (prog) prog.style.display = 'none';
        if (bar)  bar.style.width    = '0';
        htmx.ajax('GET', '/create/partials/music', { target: '#creator-panel', swap: 'innerHTML' });
        if (typeof toast === 'function') toast('Track uploaded');
      }, 800);
    } else {
      if (stat) stat.textContent = 'Upload failed';
      if (typeof toast === 'function') toast(xhr.responseText || 'Upload failed', 'error');
    }
    if (btn) { btn.disabled = false; btn.textContent = 'Upload track'; }
  };
  xhr.onerror = function() {
    if (stat) stat.textContent = 'Network error';
    if (btn)  { btn.disabled = false; btn.textContent = 'Upload track'; }
  };
  xhr.send(fd);
}

// ── Track delete ───────────────────────────────────────────────────────────────
function crtDeleteTrack(trackID) {
  if (!confirm('Delete this track? This cannot be undone.')) return;
  var fd = new FormData();
  fd.append('track_id', trackID);
  fetch('/api/track/delete', { method: 'POST', body: fd })
    .then(function(r) {
      if (r.ok) {
        var row = document.getElementById('stu-track-' + trackID);
        if (row) row.remove();
        if (typeof toast === 'function') toast('Track deleted');
      } else {
        if (typeof toast === 'function') toast('Could not delete track', 'error');
      }
    });
}
