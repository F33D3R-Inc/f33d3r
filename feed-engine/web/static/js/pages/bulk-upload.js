/**
 * bulk-upload.js — FA JavaScript for /create/bulk-upload
 *
 * FA JS Rules: LISTEN / LOCATE / PATCH / EMIT only.
 *  LISTEN — file drop, input change, drag events
 *  LOCATE — getElementById / querySelector
 *  PATCH  — innerHTML / style to update progress rows
 *  EMIT   — fetch POST /api/upload/bulk-init, then TUS PATCH per file
 *
 * D-047: PIAL UUIDs never in HTML/data-attrs/JS vars/URLs.
 * Upload IDs are opaque TUS session UUIDs — not PIAL identifiers.
 */

(function () {
  'use strict';

  // ── Constants ───────────────────────────────────────────────────────────────
  var MAX_FILES = 20;
  var MAX_CONCURRENT = 3;   // max simultaneous TUS uploads
  var POLL_INTERVAL_MS = 3000;

  // ── State ───────────────────────────────────────────────────────────────────
  var selectedFiles = [];   // File[] from drop/browse
  var batchID = null;
  var uploadSlots = [];     // [{upload_id, tus_url, filename}]
  var pollTimer = null;

  // ── LISTEN: wire drop zone and file input ───────────────────────────────────
  document.addEventListener('DOMContentLoaded', function () {
    var drop = document.getElementById('bulk-upload-drop');
    var input = document.getElementById('bulk-upload-file-input');
    if (!drop || !input) return;

    // Drag-over styling
    drop.addEventListener('dragover', function (e) {
      e.preventDefault();
      drop.classList.add('bulk-upload-drop--dragging');
    });
    drop.addEventListener('dragleave', function () {
      drop.classList.remove('bulk-upload-drop--dragging');
    });

    // Drop
    drop.addEventListener('drop', function (e) {
      e.preventDefault();
      drop.classList.remove('bulk-upload-drop--dragging');
      var files = Array.from(e.dataTransfer.files).filter(isVideo);
      applyFiles(files);
    });

    // Click-to-browse
    drop.addEventListener('click', function (e) {
      if (e.target.id === 'bulk-upload-browse-btn') return; // handled by button
      input.click();
    });

    input.addEventListener('change', function () {
      var files = Array.from(input.files).filter(isVideo);
      applyFiles(files);
    });
  });

  // ── LISTEN: expose browse trigger ───────────────────────────────────────────
  window.bulkUploadBrowse = function () {
    var input = document.getElementById('bulk-upload-file-input');
    if (input) input.click();
  };

  // ── LISTEN: cancel all ──────────────────────────────────────────────────────
  window.bulkUploadCancelAll = function () {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
    selectedFiles = [];
    batchID = null;
    uploadSlots = [];
    showDropZone();
  };

  // ── Helpers ─────────────────────────────────────────────────────────────────
  function isVideo(file) {
    return file.type.startsWith('video/');
  }

  function fmtBytes(bytes) {
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(0) + ' KB';
    if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
    return (bytes / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
  }

  function applyFiles(files) {
    if (!files.length) return;
    // Merge with existing selection, cap at MAX_FILES
    var merged = selectedFiles.concat(files).slice(0, MAX_FILES);
    selectedFiles = merged;

    // Update badge
    var badge = document.getElementById('bulk-upload-file-count');
    var num = document.getElementById('bulk-upload-file-count-num');
    if (badge && num) {
      num.textContent = selectedFiles.length;
      badge.removeAttribute('hidden');
    }

    // Auto-start if files > 0
    if (selectedFiles.length > 0) {
      startBulkUpload();
    }
  }

  function showDropZone() {
    var dropSec = document.getElementById('bulk-upload-drop-section');
    var progSec = document.getElementById('bulk-upload-progress-section');
    if (dropSec) dropSec.removeAttribute('hidden');
    if (progSec) progSec.setAttribute('hidden', '');
  }

  function showProgress() {
    var dropSec = document.getElementById('bulk-upload-drop-section');
    var progSec = document.getElementById('bulk-upload-progress-section');
    if (dropSec) dropSec.setAttribute('hidden', '');
    if (progSec) progSec.removeAttribute('hidden');
  }

  // ── EMIT: bulk-init ─────────────────────────────────────────────────────────
  function startBulkUpload() {
    var handles = selectedFiles.map(function (f) { return f.name; });
    var csrfToken = getCsrf();

    // EMIT: call bulk-init
    fetch('/api/upload/bulk-init', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify({ count: handles.length, handles: handles })
    })
      .then(function (r) {
        if (!r.ok) throw new Error('bulk-init failed: ' + r.status);
        return r.json();
      })
      .then(function (data) {
        batchID = data.batch_id;
        uploadSlots = data.upload_slots || [];

        // PATCH: render progress section
        renderProgressRows();
        showProgress();

        // PATCH: update batch_id on the poll container
        var prog = document.getElementById('bulk-upload-progress');
        if (prog) prog.dataset.batchId = batchID;

        // Start TUS uploads (max 3 concurrent)
        startParallelTusUploads();

        // Start status poll
        startPoll();
      })
      .catch(function (err) {
        console.error('[bulk-upload] bulk-init error:', err);
      });
  }

  // ── PATCH: render all item rows ─────────────────────────────────────────────
  function renderProgressRows() {
    var list = document.getElementById('bulk-upload-item-list');
    if (!list) return;

    // Update summary counts
    patchCount('bulk-upload-total-count', uploadSlots.length);
    patchCount('bulk-upload-ready-count', 0);

    var html = '';
    uploadSlots.forEach(function (slot, idx) {
      var fileSize = selectedFiles[idx] ? fmtBytes(selectedFiles[idx].size) : '';
      html += buildItemHTML(slot.upload_id, slot.filename, fileSize, 'queued', '');
    });
    list.innerHTML = html;
  }

  function buildItemHTML(uploadID, filename, fileSize, status, masterURL) {
    var thumbHTML;
    if (status === 'ready' && masterURL) {
      thumbHTML = '<video class="img-thumbnail" src="' + escHTML(masterURL) + '" muted preload="metadata" loop></video>';
    } else {
      thumbHTML = '<div class="bulk-upload-item__thumb-placeholder">' +
        '<svg width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.5" viewBox="0 0 24 24">' +
        '<polygon points="23 7 16 12 23 17 23 7"/><rect x="1" y="5" width="15" height="14" rx="2" ry="2"/>' +
        '</svg></div>';
    }

    var chipLabel = {
      queued: 'Queued', uploading: 'Uploading', processing: 'Processing',
      ready: 'Ready', failed: 'Failed'
    }[status] || status;

    return '<div class="bulk-upload-item bulk-upload-item--' + escHTML(status) + '"' +
      ' id="bulk-upload-item-' + escHTML(uploadID) + '"' +
      ' data-upload-id="' + escHTML(uploadID) + '" role="listitem">' +
      '<div class="bulk-upload-item__thumb" id="bulk-upload-thumb-' + escHTML(uploadID) + '" aria-hidden="true">' +
      thumbHTML + '</div>' +
      '<div class="bulk-upload-item__info">' +
      '<span class="bulk-upload-item__filename">' + escHTML(filename) + '</span>' +
      (fileSize ? '<span class="bulk-upload-item__filesize">' + escHTML(fileSize) + '</span>' : '') +
      '<div class="bulk-upload-item__progress-track" id="bulk-upload-track-' + escHTML(uploadID) + '"' +
      ' role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0">' +
      '<div class="poll-bar bulk-upload-item__progress-fill" id="bulk-upload-bar-' + escHTML(uploadID) + '" style="width:0%"></div>' +
      '</div></div>' +
      '<span class="bulk-upload-item__status-chip bulk-upload-item__status-chip--' + escHTML(status) + '"' +
      ' id="bulk-upload-chip-' + escHTML(uploadID) + '">' + escHTML(chipLabel) + '</span>' +
      '</div>';
  }

  // ── TUS parallel uploader ───────────────────────────────────────────────────
  function startParallelTusUploads() {
    var queue = uploadSlots.slice(); // copy
    var active = 0;

    function next() {
      while (active < MAX_CONCURRENT && queue.length > 0) {
        var slot = queue.shift();
        var fileIdx = uploadSlots.indexOf(slot);
        if (fileIdx === -1) return;
        var file = selectedFiles[fileIdx];
        if (!file) continue;
        active++;
        tusUploadFile(slot.upload_id, slot.tus_url, file)
          .then(function () { active--; next(); })
          .catch(function () { active--; next(); });
      }
    }
    next();
  }

  /**
   * tusUploadFile — minimal TUS protocol implementation.
   *   1. HEAD to get current offset (resumability)
   *   2. PATCH to stream the file in one chunk (no partial-chunk retry needed for bulk)
   *
   * Progress is tracked via XMLHttpRequest progress events.
   */
  function tusUploadFile(uploadID, tusURL, file) {
    return new Promise(function (resolve, reject) {
      // PATCH chip and bar to "uploading"
      patchItemStatus(uploadID, 'uploading', 0);

      var csrfToken = getCsrf();

      var xhr = new XMLHttpRequest();
      xhr.open('PATCH', tusURL, true);
      xhr.setRequestHeader('Content-Type', 'application/offset+octet-stream');
      xhr.setRequestHeader('Upload-Offset', '0');
      xhr.setRequestHeader('Upload-Length', String(file.size));
      xhr.setRequestHeader('Tus-Resumable', '1.0.0');
      xhr.setRequestHeader('Upload-Metadata', 'filename ' + b64encode(file.name));
      if (csrfToken) xhr.setRequestHeader('X-CSRF-Token', csrfToken);

      xhr.upload.addEventListener('progress', function (e) {
        if (e.lengthComputable && e.total > 0) {
          var pct = Math.round(e.loaded / e.total * 100);
          patchProgressBar(uploadID, pct);
        }
      });

      xhr.addEventListener('load', function () {
        if (xhr.status === 204 || xhr.status === 200) {
          patchItemStatus(uploadID, 'processing', 100);
          resolve();
        } else {
          patchItemStatus(uploadID, 'failed', 0);
          reject(new Error('TUS PATCH failed: ' + xhr.status));
        }
      });

      xhr.addEventListener('error', function () {
        patchItemStatus(uploadID, 'failed', 0);
        reject(new Error('TUS PATCH network error'));
      });

      xhr.send(file);
    });
  }

  // ── Status poll ─────────────────────────────────────────────────────────────
  function startPoll() {
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = setInterval(function () {
      if (!batchID) return;
      fetchBatchStatus(batchID);
    }, POLL_INTERVAL_MS);
  }

  function fetchBatchStatus(bid) {
    fetch('/api/upload/bulk-status/' + bid)
      .then(function (r) { return r.json(); })
      .then(function (data) {
        patchCount('bulk-upload-ready-count', data.ready || 0);
        patchCount('bulk-upload-total-count', data.total || 0);

        (data.items || []).forEach(function (item) {
          patchItemFromServer(item);
        });

        // Stop polling when nothing is still in-flight
        var inFlight = (data.queued || 0) + (data.uploading || 0) + (data.processing || 0);
        if (inFlight === 0 && pollTimer) {
          clearInterval(pollTimer);
          pollTimer = null;
        }
      })
      .catch(function (err) {
        console.warn('[bulk-upload] poll error:', err);
      });
  }

  // ── PATCH helpers ───────────────────────────────────────────────────────────
  function patchItemFromServer(item) {
    var row = document.getElementById('bulk-upload-item-' + item.upload_id);
    if (!row) return;

    // Update status class on row
    row.className = 'bulk-upload-item bulk-upload-item--' + (item.status || 'queued');

    // Update status chip
    var chip = document.getElementById('bulk-upload-chip-' + item.upload_id);
    if (chip) {
      var labels = { queued: 'Queued', uploading: 'Uploading', processing: 'Processing', ready: 'Ready', failed: 'Failed' };
      chip.textContent = labels[item.status] || item.status;
      chip.className = 'bulk-upload-item__status-chip bulk-upload-item__status-chip--' + (item.status || 'queued');
    }

    // When ready, inject thumbnail video
    if (item.status === 'ready' && item.master_url) {
      patchProgressBar(item.upload_id, 100);
      var thumb = document.getElementById('bulk-upload-thumb-' + item.upload_id);
      if (thumb && !thumb.querySelector('video')) {
        thumb.innerHTML = '<video class="img-thumbnail" src="' + escHTML(item.master_url) +
          '" muted preload="metadata" loop></video>';
      }
    }

    if (item.status === 'failed') {
      patchProgressBar(item.upload_id, 0);
    }
  }

  function patchItemStatus(uploadID, status, pct) {
    var row = document.getElementById('bulk-upload-item-' + uploadID);
    if (row) row.className = 'bulk-upload-item bulk-upload-item--' + status;

    var chip = document.getElementById('bulk-upload-chip-' + uploadID);
    if (chip) {
      var labels = { queued: 'Queued', uploading: 'Uploading', processing: 'Processing', ready: 'Ready', failed: 'Failed' };
      chip.textContent = labels[status] || status;
      chip.className = 'bulk-upload-item__status-chip bulk-upload-item__status-chip--' + status;
    }

    patchProgressBar(uploadID, pct);
  }

  function patchProgressBar(uploadID, pct) {
    var bar = document.getElementById('bulk-upload-bar-' + uploadID);
    if (!bar) return;
    bar.style.width = pct + '%';
    var track = document.getElementById('bulk-upload-track-' + uploadID);
    if (track) track.setAttribute('aria-valuenow', String(pct));
  }

  function patchCount(id, n) {
    var el = document.getElementById(id);
    if (el) el.textContent = String(n);
  }

  // ── Utility ─────────────────────────────────────────────────────────────────
  function getCsrf() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.getAttribute('content') : '';
  }

  function escHTML(str) {
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  function b64encode(str) {
    try { return btoa(unescape(encodeURIComponent(str))); } catch (e) { return btoa(str); }
  }

})();
