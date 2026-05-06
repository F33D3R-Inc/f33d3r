// f33d3r.js — Application logic (external, no template tags, no inline execution)
// All user data is read from <meta> or data- attributes on <body>.
// No JSON polling. Real-time via SSE (/api/events). No fetch() for data queries.

'use strict';

// ── Theme: read accent from body data attribute ───────────────────────────────
(function() {
  var app = document.getElementById('f33d3r-app');
  if (!app) return;
  var accent = app.dataset.accent;
  if (accent && /^#[0-9a-fA-F]{6}$/.test(accent)) {
    app.style.setProperty('--accent', accent);
    app.style.setProperty('--accent-muted', accent + 'AA');
  }
})();

// ── SSE: connect to /api/events for real-time notifications ───────────────────
// The Go server pushes HTML fragments. HTMX swaps them into the DOM.
// No polling. No fetch(). No JSON.
(function() {
  var pialMeta = document.querySelector('meta[name="f33d3r:pial"]');
  if (!pialMeta) return; // not logged in

  var es = new EventSource('/api/events');
  // Expose globally so video-player.js can close on pagehide (memory hygiene).
  window.__F33D3R_SSE__ = es;

  es.addEventListener('notify', function(e) {
    // Update badge counter in sidebar/nav
    document.querySelectorAll('.sse-notif-target').forEach(function(el) {
      el.innerHTML = e.data;
    });
    // On mobile: show scrolling ticker above the bottom nav
    if (window.innerWidth < 1024 && e.data && e.data.trim() !== '0') {
      showMobileNotifTicker('You have new notifications');
    }
  });

  es.addEventListener('dm', function(e) {
    if (window.innerWidth < 1024 && e.data) {
      showMobileNotifTicker('New message: ' + e.data);
    }
  });

  es.addEventListener('balance', function(e) {
    if (!e.data) return;
    var el = document.getElementById('sidebar-aet-bal');
    if (el) el.textContent = e.data;
    var r = document.getElementById('right-aet-bal');
    if (r) r.textContent = e.data;
  });

  es.onerror = function() {
    // SSE reconnects automatically — no action needed
  };
})();

// ── Compose modal ─────────────────────────────────────────────────────────────
window.closeCompose = function() {
  document.getElementById('compose-modal').classList.add('hidden');
  _clearThreadSegments();
};
window.openCompose = function() {
  document.getElementById('compose-modal').classList.remove('hidden');
  setTimeout(function() {
    var ta = document.getElementById('compose-textarea');
    if (ta) ta.focus();
  }, 80);
};

document.addEventListener('DOMContentLoaded', function() {
  // Navigation progress bar — uses #nav-progress from base.html
  var _npbar = (function(){
    var bar = document.getElementById('nav-progress');
    if (!bar) { bar = document.createElement('div'); document.body.appendChild(bar); }
    var t;
    return {
      start: function(){ clearTimeout(t); bar.style.opacity='1'; bar.style.width='72%'; },
      done:  function(){ bar.style.width='100%'; t=setTimeout(function(){ bar.style.opacity='0'; bar.style.width='0'; },320); }
    };
  })();
  document.body.addEventListener('htmx:beforeRequest', function(){ _npbar.start(); });
  document.body.addEventListener('htmx:afterSwap',     function(){ _npbar.done(); });
  document.body.addEventListener('htmx:responseError', function(){ _npbar.done(); });

  // Compose open buttons
  var fab = document.getElementById('post-fab');
  var mobBtn = document.getElementById('mobile-post-btn');
  if (fab)    fab.addEventListener('click', openCompose);
  if (mobBtn) mobBtn.addEventListener('click', openCompose);

  // Compose modal: backdrop click closes
  var overlay = document.getElementById('compose-modal');
  if (overlay) overlay.addEventListener('click', function(e) {
    if (e.target === overlay) closeCompose();
  });

  // Compose close button
  var closeBtn = document.getElementById('compose-close-btn');
  if (closeBtn) closeBtn.addEventListener('click', closeCompose);

  // Compose post button
  var postBtn = document.getElementById('compose-post-btn');
  if (postBtn) postBtn.addEventListener('click', submitCompose);

  // Textarea input
  var ta = document.getElementById('compose-textarea');
  if (ta) ta.addEventListener('input', function() {
    onComposeInput(ta);
    updateCharCount();
  });

  // Media file input
  var mediaInput = document.getElementById('media-file-input');
  if (mediaInput) mediaInput.addEventListener('change', function() { onMediaSelect(mediaInput); });

  // Compose action buttons
  var pollBtn = document.getElementById('compose-poll-btn');
  if (pollBtn) pollBtn.addEventListener('click', togglePollCompose);

  var gifBtn = document.getElementById('compose-gif-btn');
  if (gifBtn) gifBtn.addEventListener('click', openGifPicker);

  var locBtn = document.getElementById('compose-location-btn');
  if (locBtn) locBtn.addEventListener('click', addComposeLocation);

  var mentionBtn = document.getElementById('compose-mention-btn');
  if (mentionBtn) mentionBtn.addEventListener('click', insertAtSymbol);

  var hashBtn = document.getElementById('compose-hashtag-btn');
  if (hashBtn) hashBtn.addEventListener('click', insertHashSymbol);

  var removeLocBtn = document.getElementById('compose-remove-location-btn');
  if (removeLocBtn) removeLocBtn.addEventListener('click', removeComposeLocation);

  // Thread compose
  var threadBtn = document.getElementById('compose-thread-btn');
  if (threadBtn) threadBtn.addEventListener('click', toggleThreadCompose);

  var threadAddBtn = document.getElementById('thread-add-btn');
  if (threadAddBtn) threadAddBtn.addEventListener('click', _addThreadSegment);

  // Poll add option button
  var pollAddBtn = document.getElementById('poll-add-btn');
  if (pollAddBtn) pollAddBtn.addEventListener('click', addPollOption);

  // Mobile more overlay
  var mobOverlay = document.getElementById('mob-more-overlay');
  if (mobOverlay) mobOverlay.addEventListener('click', closeMobileMore);

  // Sidebar user menu
  var sidebarUserBtn = document.getElementById('sidebar-user-btn');
  if (sidebarUserBtn) sidebarUserBtn.addEventListener('click', toggleUserMenu);

  // Mobile more button
  var mobMoreBtn = document.getElementById('mob-more-btn');
  if (mobMoreBtn) mobMoreBtn.addEventListener('click', toggleMobileMore);
});

window.updateCharCount = function() {
  var ta = document.getElementById('compose-textarea');
  var ring = document.getElementById('char-ring');
  var inner = document.getElementById('char-count-inner');
  if (!ta || !ring) return;
  var used = ta.value.length;
  var max = 25000;
  var pct = used / max;
  var circumference = 56.5;
  ring.style.strokeDashoffset = String(circumference * (1 - pct));
  ring.style.stroke = pct > 0.9 ? '#ef4444' : pct > 0.7 ? '#f97316' : 'var(--accent)';
  if (inner) {
    inner.style.display = used > 24500 ? 'flex' : 'none';
    if (used > 24500) inner.textContent = String(max - used);
  }
};

window.submitCompose = async function() {
  if (_threadMode) { return submitThreadCompose(); }
  var ta  = document.getElementById('compose-textarea');
  var btn = document.getElementById('compose-post-btn');
  var gating = (document.getElementById('compose-gating') || {}).value || 'open';
  if (!ta || !ta.value.trim()) return;
  btn.disabled = true; btn.textContent = 'Posting…';
  var fd = new FormData();
  fd.append('content', ta.value);
  fd.append('comment_gating', gating);
  document.querySelectorAll('#compose-modal [name="media_urls"]').forEach(function(i) {
    fd.append('media_urls', i.value);
  });
  // Sprint 0 / S0.6 — video asset fields
  document.querySelectorAll('#compose-modal input[data-video-asset="1"]').forEach(function(i) {
    fd.append(i.name, i.value);
  });
  var locLabel = document.getElementById('compose-location-label');
  if (locLabel && locLabel.textContent) fd.append('location_tag', locLabel.textContent);
  var nsfwChk = document.getElementById('compose-nsfw');
  if (nsfwChk && nsfwChk.checked) fd.append('is_nsfw', '1');
  try {
    var r = await fetch('/api/post', { method: 'POST', body: fd, headers: { 'HX-Request': 'true' } });
    if (r.ok) {
      var html = await r.text();
      toast('Post published');
      ta.value = '';
      updateCharCount();
      clearMediaPreviews();
      removeComposeLocation();
      if (nsfwChk) nsfwChk.checked = false;
      closeCompose();
      // Immediately prepend post card to feed
      if (html && html.trim()) {
        var fc = document.getElementById('feed-container');
        if (fc) {
          var tmp = document.createElement('div');
          tmp.innerHTML = html.trim();
          var article = tmp.querySelector('article');
          if (article) {
            article.style.background = 'color-mix(in srgb,var(--accent) 6%,var(--panel))';
            fc.insertBefore(article, fc.firstChild);
            window.scrollTo({top:0, behavior:'smooth'});
            if (typeof formatPostTimes === 'function') formatPostTimes(fc);
            htmx.process(article);
            setTimeout(function() { article.style.transition = 'background 1.2s ease'; article.style.background = ''; }, 50);
          }
        }
      }
      htmx.trigger(document.body, 'postCreated');
    } else {
      var t = await r.text();
      toast(t || 'Post failed', 'error');
    }
  } catch(e) { toast('Post failed: ' + e.message, 'error'); }
  btn.disabled = false; btn.textContent = 'Post';
};

// ── Compose helpers ───────────────────────────────────────────────────────────
window.insertAtSymbol = function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  var pos = ta.selectionStart;
  ta.value = ta.value.slice(0, pos) + '@' + ta.value.slice(pos);
  ta.selectionStart = ta.selectionEnd = pos + 1;
  ta.focus();
  onComposeInput(ta);
};
window.insertHashSymbol = function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  var pos = ta.selectionStart;
  ta.value = ta.value.slice(0, pos) + '#' + ta.value.slice(pos);
  ta.selectionStart = ta.selectionEnd = pos + 1;
  ta.focus();
};
window.addComposeLocation = function() {
  if (!navigator.geolocation) { toast('Geolocation not supported', 'error'); return; }
  navigator.geolocation.getCurrentPosition(async function(pos) {
    try {
      var r = await fetch('https://nominatim.openstreetmap.org/reverse?lat=' + pos.coords.latitude + '&lon=' + pos.coords.longitude + '&format=json');
      var d = await r.json();
      var city    = (d.address || {}).city || (d.address || {}).town || (d.address || {}).village || '';
      var country = (d.address || {}).country || '';
      var loc = [city, country].filter(Boolean).join(', ');
      if (loc) {
        var label = document.getElementById('compose-location-label');
        var tag   = document.getElementById('compose-location-tag');
        if (label) label.textContent = loc;
        if (tag)   tag.style.display = '';
      }
    } catch(e) { toast('Could not detect location', 'error'); }
  }, function() { toast('Location access denied', 'error'); }, { timeout: 8000 });
};
window.removeComposeLocation = function() {
  var label = document.getElementById('compose-location-label');
  var tag   = document.getElementById('compose-location-tag');
  if (label) label.textContent = '';
  if (tag)   tag.style.display = 'none';
};
window.openGifPicker = function() {
  toast('GIF search coming soon', 'info');
};

// ── Mention autocomplete ──────────────────────────────────────────────────────
var _mentionTimer = null;
window.onComposeInput = function(ta) {
  var dd = document.getElementById('mention-dropdown');
  var cursor = ta.selectionStart;
  var text = ta.value.slice(0, cursor);
  var match = text.match(/@([a-zA-Z0-9_\-]*)$/);
  if (!match) { if (dd) dd.style.display = 'none'; return; }
  var q = match[1];
  clearTimeout(_mentionTimer);
  _mentionTimer = setTimeout(async function() {
    if (q.length < 1) { dd.style.display = 'none'; return; }
    try {
      var res = await fetch('/api/users/search?q=' + encodeURIComponent(q));
      var users = await res.json();
      if (!users || !users.length) { dd.style.display = 'none'; return; }
      var myHandle = (document.body.dataset.handle || '').toLowerCase();
      var filtered = users.filter(function(u) { return !myHandle || u.handle.toLowerCase() !== myHandle; });
      if (!filtered.length) { dd.style.display = 'none'; return; }
      dd.innerHTML = filtered.map(function(u, i) {
        return '<button type="button" data-mention-idx="' + i + '" onclick="insertMention(\'@' + u.handle + '\')" style="display:flex;align-items:center;gap:10px;width:100%;padding:10px 14px;background:none;border:none;border-bottom:1px solid var(--border);cursor:pointer;text-align:left"><div style="width:30px;height:30px;border-radius:50%;overflow:hidden;flex-shrink:0;' + (u.avatar_url ? '' : 'background:linear-gradient(135deg,var(--accent),color-mix(in srgb,var(--accent) 60%,#000));display:flex;align-items:center;justify-content:center;color:#fff;font-size:12px;font-weight:700') + '">' + (u.avatar_url ? '<img src="' + u.avatar_url + '" style="width:100%;height:100%;object-fit:cover">' : u.handle[0].toUpperCase()) + '</div><div><p style="font-size:13px;font-weight:500;color:var(--text-primary);margin:0">' + u.display_name + '</p><p style="font-size:11px;color:var(--text-muted);margin:0">@' + u.handle + '</p></div></button>';
      }).join('');
      dd.style.display = 'block';
    } catch(e) { dd.style.display = 'none'; }
  }, 180);
};
window.insertMention = function(handle) {
  var ta = document.getElementById('compose-textarea');
  var dd = document.getElementById('mention-dropdown');
  if (!ta) return;
  var cursor  = ta.selectionStart;
  var before  = ta.value.slice(0, cursor);
  var after   = ta.value.slice(cursor);
  var replaced = before.replace(/@[a-zA-Z0-9_\-]*$/, handle + ' ');
  ta.value = replaced + after;
  ta.selectionStart = ta.selectionEnd = replaced.length;
  ta.focus();
  if (dd) dd.style.display = 'none';
  updateCharCount();
};
document.addEventListener('keydown', function(e) {
  var dd = document.getElementById('mention-dropdown');
  if (!dd || dd.style.display === 'none') return;
  var btns = Array.from(dd.querySelectorAll('button[data-mention-idx]'));
  if (!btns.length) return;
  var active = dd.querySelector('button.mention-active');
  var idx = active ? parseInt(active.dataset.mentionIdx) : -1;
  if      (e.key === 'ArrowDown')          { e.preventDefault(); idx = Math.min(idx + 1, btns.length - 1); }
  else if (e.key === 'ArrowUp')            { e.preventDefault(); idx = Math.max(idx - 1, 0); }
  else if (e.key === 'Enter')              { e.preventDefault(); (active || btns[0])?.click(); return; }
  else if (e.key === 'Escape')             { dd.style.display = 'none'; return; }
  else return;
  btns.forEach(function(b) { b.classList.remove('mention-active'); b.style.background = ''; });
  btns[idx].classList.add('mention-active');
  btns[idx].style.background = 'var(--panel-hover)';
});
document.addEventListener('click', function(e) {
  var dd = document.getElementById('mention-dropdown');
  if (dd && !dd.contains(e.target)) dd.style.display = 'none';
});

// ── Media upload ──────────────────────────────────────────────────────────────
// The /upload/post-media endpoint returns one of:
//   image: { kind: "image", url }
//   video: { kind: "video", master_url, poster_url, duration_secs, width, height }
// (Sprint 0 / S0.6.) Videos block on Caeor's HLS transcode — show a spinner.

window.onMediaSelect = async function(input) {
  var files = Array.from(input.files);
  var modal = document.getElementById('compose-modal');
  var prev  = document.getElementById('media-previews');
  var postBtn = document.getElementById('compose-post-btn');
  var isVideo = function(f) { return /^video\//i.test(f.type) || /\.(mp4|mov|webm|mkv|m4v)$/i.test(f.name); };

  for (var i = 0; i < files.length; i++) {
    var file = files[i];
    var video = isVideo(file);

    // Lock Post button and show upload status in the compose area.
    if (postBtn) { postBtn.disabled = true; postBtn.textContent = video ? 'Processing…' : 'Uploading…'; }

    // Placeholder tile with spinner — shows immediately so user sees activity.
    var wrap = document.createElement('div');
    wrap.style.cssText = 'position:relative;width:72px;height:72px;border-radius:10px;overflow:hidden;border:1px solid var(--border);flex-shrink:0;background:var(--panel);display:flex;flex-direction:column;align-items:center;justify-content:center;gap:4px';
    wrap.innerHTML = '<div style="width:22px;height:22px;border:2px solid var(--border);border-top-color:var(--accent);border-radius:50%;animation:spin 0.7s linear infinite"></div>'
      + (video ? '<span style="font-size:8px;font-weight:700;letter-spacing:.05em;color:var(--accent);text-transform:uppercase">Processing</span>' : '');
    if (prev) prev.appendChild(wrap);

    var fd = new FormData();
    fd.append('media', file);
    try {
      var res = await fetch('/upload/post-media', { method: 'POST', body: fd });
      if (!res.ok) {
        wrap.remove();
        var errText = await res.text().catch(function() { return ''; });
        toast((errText || 'Upload failed') + ': ' + file.name, 'error');
        continue;
      }
      var data = await res.json();
      var kind = data.kind || (data.url ? 'image' : 'unknown');

      if (kind === 'video' && data.master_url) {
        _stashVideoFields(modal, data);
        _renderVideoTile(wrap, data, modal);
      } else if (kind === 'image' || data.url) {
        _stashMediaURL(modal, data.url);
        _renderImageTile(wrap, data.url, modal);
      } else {
        wrap.remove();
        toast('Upload returned an unexpected response', 'error');
      }
    } catch (e) {
      wrap.remove();
      toast('Upload failed: ' + file.name, 'error');
    }
  }

  // Restore Post button regardless of outcome.
  if (postBtn) { postBtn.disabled = false; postBtn.textContent = 'Post'; }
  input.value = '';
};

// ── Hidden-field helpers (one source of truth for compose state) ─────────────

function _stashMediaURL(modal, url) {
  if (!url || url === 'undefined' || url === 'null') return;
  var h = document.createElement('input');
  h.type = 'hidden'; h.name = 'media_urls'; h.value = url;
  h.dataset.mediaUrl = url;
  modal.appendChild(h);
}

function _stashVideoFields(modal, data) {
  // Only one video per post for now — replace any existing.
  modal.querySelectorAll('input[data-video-asset="1"]').forEach(function (n) { n.remove(); });
  var pairs = [
    ['video_master_url',    data.master_url],
    ['video_poster_url',    data.poster_url || ''],
    ['video_duration_secs', String(data.duration_secs || 0)],
    ['video_width',         String(data.width || 0)],
    ['video_height',        String(data.height || 0)],
  ];
  pairs.forEach(function (kv) {
    var h = document.createElement('input');
    h.type = 'hidden'; h.name = kv[0]; h.value = kv[1];
    h.dataset.videoAsset = '1';
    modal.appendChild(h);
  });
}

function _renderImageTile(wrap, url, modal) {
  wrap.innerHTML = '<img src="' + url + '" style="width:100%;height:100%;object-fit:cover">';
  wrap.appendChild(_makeRemoveButton(function () {
    var h = modal.querySelector('[data-media-url="' + url + '"]');
    if (h) h.remove();
    wrap.remove();
  }));
}

function _renderVideoTile(wrap, data, modal) {
  var poster = data.poster_url || '';
  wrap.innerHTML =
    (poster ? '<img src="' + poster + '" style="width:100%;height:100%;object-fit:cover">' : '') +
    '<span style="position:absolute;bottom:3px;left:3px;font-size:9px;font-weight:700;letter-spacing:.05em;color:#fff;background:rgba(0,0,0,.65);padding:1px 5px;border-radius:3px">VIDEO</span>';
  wrap.appendChild(_makeRemoveButton(function () {
    modal.querySelectorAll('input[data-video-asset="1"]').forEach(function (n) { n.remove(); });
    wrap.remove();
  }));
}

function _makeRemoveButton(onClick) {
  var rm = document.createElement('button');
  rm.type = 'button'; rm.textContent = '\xD7';
  rm.style.cssText = 'position:absolute;top:3px;right:3px;width:20px;height:20px;border-radius:50%;background:rgba(0,0,0,.65);color:#fff;font-size:14px;line-height:20px;text-align:center;border:none;cursor:pointer;padding:0';
  rm.onclick = onClick;
  return rm;
}

window.clearMediaPreviews = function() {
  var prev = document.getElementById('media-previews');
  if (prev) prev.innerHTML = '';
  document.querySelectorAll('#compose-modal [name="media_urls"]').forEach(function(el) { el.remove(); });
  document.querySelectorAll('#compose-modal input[data-video-asset="1"]').forEach(function(el) { el.remove(); });
};

// ── Thread compose ────────────────────────────────────────────────────────────
var _threadMode = false;

window.toggleThreadCompose = function() {
  _threadMode = !_threadMode;
  var segs  = document.getElementById('thread-segments');
  var addBtn = document.getElementById('thread-add-btn');
  var threadBtn = document.getElementById('compose-thread-btn');
  if (!segs || !addBtn || !threadBtn) return;
  segs.style.display  = _threadMode ? '' : 'none';
  addBtn.style.display = _threadMode ? '' : 'none';
  threadBtn.style.color = _threadMode ? 'var(--accent)' : '';
  if (_threadMode && segs.children.length === 0) {
    _addThreadSegment();
  }
};

function _addThreadSegment() {
  var segs = document.getElementById('thread-segments');
  if (!segs) return;
  var idx = segs.children.length + 1;
  var wrap = document.createElement('div');
  wrap.style.cssText = 'display:flex;gap:10px;align-items:flex-start;margin-top:8px;padding-left:0';
  wrap.innerHTML =
    '<div style="display:flex;flex-direction:column;align-items:center;width:40px;flex-shrink:0">' +
      '<div style="width:2px;height:16px;background:var(--border-soft);margin-bottom:4px"></div>' +
    '</div>' +
    '<textarea class="compose-textarea thread-seg-ta" rows="2" maxlength="25000" ' +
      'placeholder="Continue your thread…" ' +
      'style="flex:1;resize:vertical;min-height:56px" ' +
      'data-thread-seg="' + idx + '"></textarea>';
  segs.appendChild(wrap);
  wrap.querySelector('textarea').focus();
}

function _clearThreadSegments() {
  _threadMode = false;
  var segs   = document.getElementById('thread-segments');
  var addBtn = document.getElementById('thread-add-btn');
  var threadBtn = document.getElementById('compose-thread-btn');
  if (segs)  { segs.innerHTML = ''; segs.style.display = 'none'; }
  if (addBtn) addBtn.style.display = 'none';
  if (threadBtn) threadBtn.style.color = '';
}

window.submitThreadCompose = async function() {
  var ta   = document.getElementById('compose-textarea');
  var btn  = document.getElementById('compose-post-btn');
  if (!ta || !ta.value.trim()) return;
  var segments = [ta.value.trim()];
  document.querySelectorAll('.thread-seg-ta').forEach(function(t) {
    var v = t.value.trim();
    if (v) segments.push(v);
  });
  if (segments.length < 2) {
    toast('Add at least one more segment to post a thread', 'error');
    return;
  }
  var gating = (document.getElementById('compose-gating') || {}).value || 'open';
  btn.disabled = true; btn.textContent = 'Posting…';
  try {
    var r = await fetch('/api/post/thread', {
      method: 'POST',
      body: JSON.stringify({ segments: segments, comment_gating: gating }),
      headers: { 'Content-Type': 'application/json', 'HX-Request': 'true' },
    });
    if (r.ok) {
      toast('Thread posted');
      ta.value = '';
      updateCharCount();
      clearMediaPreviews();
      _clearThreadSegments();
      closeCompose();
      htmx.trigger(document.body, 'postCreated');
    } else {
      var t = await r.text();
      toast(t || 'Thread post failed', 'error');
    }
  } catch(e) { toast('Thread post failed: ' + e.message, 'error'); }
  btn.disabled = false; btn.textContent = 'Post';
};

// ── User menu ─────────────────────────────────────────────────────────────────
window.toggleUserMenu = function() {
  var p = document.getElementById('user-menu-popup');
  if (p) p.classList.toggle('hidden');
};
document.addEventListener('click', function(e) {
  var wrap  = document.getElementById('sidebar-user-wrap');
  var popup = document.getElementById('user-menu-popup');
  if (wrap && popup && !wrap.contains(e.target)) popup.classList.add('hidden');
});

// ── Mobile drawer ─────────────────────────────────────────────────────────────
window.toggleMobileMore = function() {
  var overlay = document.getElementById('mob-more-overlay');
  var drawer  = document.getElementById('mob-more-drawer');
  if (!overlay || !drawer) return;
  if (drawer.style.display !== 'none') { closeMobileMore(); }
  else { overlay.style.display = ''; drawer.style.display = ''; }
};
window.closeMobileMore = function() {
  var overlay = document.getElementById('mob-more-overlay');
  var drawer  = document.getElementById('mob-more-drawer');
  if (overlay) overlay.style.display = 'none';
  if (drawer)  drawer.style.display  = 'none';
};

// ── Poll compose ──────────────────────────────────────────────────────────────
var _pollMode = false;
var _origSubmitCompose = null;

window.togglePollCompose = function() {
  _pollMode = !_pollMode;
  var panel = document.getElementById('poll-compose-panel');
  if (panel) {
    panel.style.display = _pollMode ? '' : 'none';
    if (_pollMode) {
      var card = document.querySelector('.compose-card');
      if (card && !card.contains(panel)) card.appendChild(panel);
    }
  }
  var btn = document.querySelector('[title="Create poll"]');
  if (btn) btn.style.background = _pollMode ? 'color-mix(in srgb,var(--accent) 15%,transparent)' : '';
};
window.addPollOption = function() {
  var list = document.getElementById('poll-options-list');
  if (!list || list.children.length >= 4) return;
  var inp = document.createElement('input');
  inp.type = 'text'; inp.className = 'poll-option-input';
  inp.placeholder = 'Option ' + (list.children.length + 1); inp.maxLength = 80;
  inp.style.cssText = "width:100%;background:rgba(255,255,255,.04);border:1px solid var(--border);border-radius:var(--radius-sm);padding:8px 12px;color:var(--text-primary);font-size:14px;font-family:'DM Sans',sans-serif;outline:none";
  list.appendChild(inp);
  if (list.children.length >= 4) { var ab = document.getElementById('poll-add-btn'); if (ab) ab.style.display = 'none'; }
};

document.addEventListener('DOMContentLoaded', function() {
  _origSubmitCompose = window.submitCompose;
  window.submitCompose = async function() {
    if (!_pollMode) { return _origSubmitCompose ? _origSubmitCompose() : null; }
    var ta  = document.getElementById('compose-textarea');
    var btn = document.getElementById('compose-post-btn');
    var question = ta ? ta.value.trim() : '';
    if (!question) { toast('Write a question first', 'error'); return; }
    var options = Array.from(document.querySelectorAll('.poll-option-input'))
      .map(function(i) { return i.value.trim(); }).filter(Boolean);
    if (options.length < 2) { toast('Add at least 2 options', 'error'); return; }
    btn.disabled = true; btn.textContent = 'Posting…';
    var fd = new FormData();
    fd.append('question', question);
    options.forEach(function(o) { fd.append('option', o); });
    var dur = document.getElementById('poll-duration');
    if (dur) fd.append('duration_hours', dur.value);
    try {
      var r = await fetch('/api/poll', { method: 'POST', body: fd, headers: { 'HX-Request': 'true' } });
      if (r.ok) {
        toast('Poll posted'); ta.value = ''; _pollMode = false;
        var panel = document.getElementById('poll-compose-panel');
        if (panel) panel.style.display = 'none';
        closeCompose();
        htmx.trigger(document.body, 'postCreated');
      } else { toast(await r.text() || 'Poll failed', 'error'); }
    } catch(e) { toast('Poll failed: ' + e.message, 'error'); }
    btn.disabled = false; btn.textContent = 'Post';
  };
});

// ── Mobile notification ticker ────────────────────────────────────────────────
var _tickerTimer = null;
window.showMobileNotifTicker = function(msg) {
  var ticker = document.getElementById('mob-notif-ticker');
  var inner = document.getElementById('mob-notif-inner');
  if (!ticker || !inner) return;
  inner.textContent = msg;
  ticker.style.display = 'block';
  ticker.style.opacity = '0';
  ticker.style.transition = 'opacity 250ms ease, transform 250ms ease';
  ticker.style.transform = 'translateX(-50%) translateY(8px)';
  requestAnimationFrame(function() {
    requestAnimationFrame(function() {
      ticker.style.opacity = '1';
      ticker.style.transform = 'translateX(-50%) translateY(0)';
    });
  });
  if (_tickerTimer) clearTimeout(_tickerTimer);
  _tickerTimer = setTimeout(function() {
    ticker.style.opacity = '0';
    ticker.style.transform = 'translateX(-50%) translateY(8px)';
    setTimeout(function() { ticker.style.display = 'none'; }, 260);
  }, 4000);
};

// ── Post timestamps: format data-ts Unix seconds to local 24h time ───────────
function formatPostTimes(root) {
  (root || document).querySelectorAll('time.post-time[data-ts]').forEach(function(el) {
    var ts = parseInt(el.dataset.ts, 10);
    if (!ts) return;
    var d = new Date(ts * 1000);
    var now = new Date();
    var diffMs = now - d;
    var hhmm = d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', hour12:false});
    var label;
    if (diffMs < 60000) {
      label = 'now';
    } else if (diffMs < 3600000) {
      label = Math.floor(diffMs / 60000) + 'm';
    } else if (diffMs < 86400000) {
      label = hhmm;
    } else if (diffMs < 7 * 86400000) {
      var day = d.toLocaleDateString([], {weekday:'short'});
      label = day + ' ' + hhmm;
    } else {
      var mon = d.toLocaleDateString([], {month:'short', day:'numeric'});
      label = mon + ' ' + hhmm;
    }
    el.textContent = label;
    el.title = d.toLocaleString([], {dateStyle:'medium', timeStyle:'short', hour12:false});
  });
}
document.addEventListener('DOMContentLoaded', function() { formatPostTimes(); });
document.addEventListener('htmx:afterSwap', function(e) { formatPostTimes(e.detail.target); });

// ── Media carousel ───────────────────────────────────────────────────────────
function mediaCarouselGoTo(carousel, idx) {
  if (!carousel) return;
  var slides  = carousel.querySelectorAll('.carousel-slide');
  var dots    = carousel.querySelectorAll('.carousel-dot');
  var counter = carousel.querySelector('.carousel-counter');
  var cur = parseInt(carousel.dataset.idx || '0', 10);
  var n   = slides.length;
  idx = ((idx % n) + n) % n;
  if (cur === idx) return;
  slides[cur].classList.remove('active');
  slides[idx].classList.add('active');
  if (dots[cur]) dots[cur].classList.remove('active');
  if (dots[idx]) dots[idx].classList.add('active');
  if (counter) counter.textContent = (idx + 1) + '/' + n;
  carousel.dataset.idx = idx;
}
function mediaCarouselStep(btn, delta) {
  var carousel = btn.closest('.media-carousel');
  if (!carousel) return;
  mediaCarouselGoTo(carousel, parseInt(carousel.dataset.idx || '0', 10) + delta);
}
(function() {
  function initTouch(carousel) {
    if (carousel._f33dTouch) return;
    carousel._f33dTouch = true;
    var sx = 0, sy = 0;
    carousel.addEventListener('touchstart', function(e) { sx = e.touches[0].clientX; sy = e.touches[0].clientY; }, {passive:true});
    carousel.addEventListener('touchend', function(e) {
      var dx = e.changedTouches[0].clientX - sx;
      var dy = e.changedTouches[0].clientY - sy;
      if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > 35)
        mediaCarouselGoTo(carousel, parseInt(carousel.dataset.idx || '0', 10) + (dx < 0 ? 1 : -1));
    }, {passive:true});
  }
  function scan(root) { (root || document).querySelectorAll('.media-carousel').forEach(initTouch); }
  document.addEventListener('DOMContentLoaded', function() { scan(); });
  document.addEventListener('htmx:afterSwap', function(e) { scan(e.detail && e.detail.target); });
})();

// ── Toast ─────────────────────────────────────────────────────────────────────
window.toast = function(msg, type) {
  type = type || 'success';
  var t = document.createElement('div');
  t.className = 'toast toast-' + type;
  t.textContent = msg;
  var container = document.getElementById('toast-container');
  if (container) container.appendChild(t);
  setTimeout(function() { t.classList.add('toast-show'); }, 10);
  setTimeout(function() { t.classList.remove('toast-show'); setTimeout(function() { t.remove(); }, 300); }, 3000);
};
