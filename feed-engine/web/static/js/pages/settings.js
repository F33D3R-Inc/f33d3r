'use strict';

// ── Settings page JS ─────────────────────────────────────────────────────────
// Handles: crop modal, bio counter, pronouns toggle, location detect,
//          theme live preview, dark mode checkbox, url auto-prefix,
//          social links, password change, data export.

// ── Crop modal state ─────────────────────────────────────────────────────────

var _cropState = {
  fileInput: null,
  type: null,
  img: new Image(),
  canvas: null,
  ctx: null,
  dragging: false,
  lastX: 0, lastY: 0,
  offsetX: 0, offsetY: 0,
  scale: 1,
  minScale: 0.5,
  _lastPinchDist: null,
};

function initCrop(fileInput, type) {
  var file = fileInput.files[0];
  if (!file) return;
  var reader = new FileReader();
  reader.onload = function(e) {
    _cropState.fileInput = fileInput;
    _cropState.type = type;
    _cropState.scale = 1;
    _cropState.offsetX = 0;
    _cropState.offsetY = 0;

    var modal = document.getElementById('settings-crop-modal');
    var canvas = document.getElementById('crop-canvas');
    var title = document.getElementById('crop-modal-title');
    _cropState.canvas = canvas;
    _cropState.ctx = canvas.getContext('2d');

    if (type === 'avatar') {
      canvas.width = 320;
      canvas.height = 320;
      title.textContent = 'Adjust photo';
    } else {
      canvas.width = 600;
      canvas.height = 200;
      title.textContent = 'Adjust banner';
    }

    var slider = document.getElementById('crop-zoom-slider');
    slider.value = 1;

    _cropState.img = new Image();
    _cropState.img.onload = function() {
      var scaleX = canvas.width / _cropState.img.width;
      var scaleY = canvas.height / _cropState.img.height;
      _cropState.scale = Math.max(scaleX, scaleY);
      _cropState.minScale = _cropState.scale * 0.8;
      slider.min = _cropState.minScale;
      slider.max = _cropState.scale * 4;
      slider.value = _cropState.scale;
      _cropDraw();
    };
    _cropState.img.src = e.target.result;

    modal.style.display = 'flex';
    document.body.style.overflow = 'hidden';
    _cropWireEvents();
  };
  reader.readAsDataURL(file);
}

function closeCropModal() {
  var modal = document.getElementById('settings-crop-modal');
  modal.style.display = 'none';
  document.body.style.overflow = '';
  _cropUnwireEvents();
  if (_cropState.fileInput) {
    _cropState.fileInput.value = '';
  }
}

function applyCrop() {
  var canvas = _cropState.canvas;
  canvas.toBlob(function(blob) {
    var ext = _cropState.type === 'avatar' ? 'avatar.jpg' : 'banner.jpg';
    var file = new File([blob], ext, { type: 'image/jpeg' });
    var dt = new DataTransfer();
    dt.items.add(file);
    _cropState.fileInput.files = dt.files;

    var url = URL.createObjectURL(blob);
    if (_cropState.type === 'avatar') {
      var el = document.getElementById('settings-avatar-img');
      if (el && el.tagName === 'IMG') {
        el.src = url;
      } else if (el) {
        var img = document.createElement('img');
        img.id = 'settings-avatar-img';
        img.src = url;
        img.style.cssText = 'width:100%;height:100%;object-fit:cover;border-radius:50%';
        el.parentNode.replaceChild(img, el);
      }
    } else {
      var banner = document.getElementById('settings-banner-preview');
      if (banner) banner.style.background = 'url(' + url + ') center/cover';
    }

    var modal = document.getElementById('settings-crop-modal');
    modal.style.display = 'none';
    document.body.style.overflow = '';
    _cropUnwireEvents();
  }, 'image/jpeg', 0.92);
}

function _cropZoom(delta) {
  var slider = document.getElementById('crop-zoom-slider');
  _cropSetScale(Math.min(Math.max(_cropState.scale + delta, _cropState.minScale), parseFloat(slider.max)));
}

function _cropSetScale(val) {
  _cropState.scale = val;
  var slider = document.getElementById('crop-zoom-slider');
  slider.value = val;
  _cropDraw();
}

function _cropDraw() {
  var s = _cropState;
  if (!s.ctx || !s.img.complete || !s.img.naturalWidth) return;
  var c = s.canvas;
  s.ctx.clearRect(0, 0, c.width, c.height);

  var w = s.img.width * s.scale;
  var h = s.img.height * s.scale;
  var x = c.width / 2 - w / 2 + s.offsetX;
  var y = c.height / 2 - h / 2 + s.offsetY;

  s.ctx.drawImage(s.img, x, y, w, h);

  if (s.type === 'avatar') {
    var r = c.width / 2 - 4;
    // Dim outside circle
    s.ctx.save();
    s.ctx.fillStyle = 'rgba(0,0,0,0.38)';
    s.ctx.fillRect(0, 0, c.width, c.height);
    s.ctx.globalCompositeOperation = 'destination-out';
    s.ctx.beginPath();
    s.ctx.arc(c.width / 2, c.height / 2, r, 0, Math.PI * 2);
    s.ctx.fill();
    s.ctx.restore();
    // Redraw image clipped to circle
    s.ctx.save();
    s.ctx.beginPath();
    s.ctx.arc(c.width / 2, c.height / 2, r, 0, Math.PI * 2);
    s.ctx.clip();
    s.ctx.drawImage(s.img, x, y, w, h);
    s.ctx.restore();
    // Circle border
    s.ctx.beginPath();
    s.ctx.arc(c.width / 2, c.height / 2, r, 0, Math.PI * 2);
    s.ctx.strokeStyle = 'rgba(255,255,255,0.55)';
    s.ctx.lineWidth = 2;
    s.ctx.stroke();
  }
}

function _onCropMouseDown(e) {
  _cropState.dragging = true;
  _cropState.lastX = e.clientX;
  _cropState.lastY = e.clientY;
}
function _onCropMouseMove(e) {
  if (!_cropState.dragging) return;
  _cropState.offsetX += e.clientX - _cropState.lastX;
  _cropState.offsetY += e.clientY - _cropState.lastY;
  _cropState.lastX = e.clientX;
  _cropState.lastY = e.clientY;
  _cropDraw();
}
function _onCropMouseUp() { _cropState.dragging = false; }
function _onCropWheel(e) {
  e.preventDefault();
  _cropZoom(e.deltaY < 0 ? 0.08 : -0.08);
}
function _onCropTouchStart(e) {
  if (e.touches.length === 1) {
    _cropState.dragging = true;
    _cropState.lastX = e.touches[0].clientX;
    _cropState.lastY = e.touches[0].clientY;
  }
  _cropState._lastPinchDist = null;
}
function _onCropTouchMove(e) {
  e.preventDefault();
  if (e.touches.length === 2) {
    var dx = e.touches[0].clientX - e.touches[1].clientX;
    var dy = e.touches[0].clientY - e.touches[1].clientY;
    var dist = Math.sqrt(dx * dx + dy * dy);
    if (_cropState._lastPinchDist) {
      _cropZoom((dist - _cropState._lastPinchDist) * 0.004);
    }
    _cropState._lastPinchDist = dist;
    return;
  }
  if (!_cropState.dragging || e.touches.length !== 1) return;
  _cropState.offsetX += e.touches[0].clientX - _cropState.lastX;
  _cropState.offsetY += e.touches[0].clientY - _cropState.lastY;
  _cropState.lastX = e.touches[0].clientX;
  _cropState.lastY = e.touches[0].clientY;
  _cropDraw();
}
function _onCropTouchEnd() { _cropState.dragging = false; }

function _cropWireEvents() {
  var c = _cropState.canvas;
  c.addEventListener('mousedown', _onCropMouseDown);
  window.addEventListener('mousemove', _onCropMouseMove);
  window.addEventListener('mouseup', _onCropMouseUp);
  c.addEventListener('wheel', _onCropWheel, { passive: false });
  c.addEventListener('touchstart', _onCropTouchStart, { passive: false });
  c.addEventListener('touchmove', _onCropTouchMove, { passive: false });
  c.addEventListener('touchend', _onCropTouchEnd);
}
function _cropUnwireEvents() {
  var c = _cropState.canvas;
  if (!c) return;
  c.removeEventListener('mousedown', _onCropMouseDown);
  window.removeEventListener('mousemove', _onCropMouseMove);
  window.removeEventListener('mouseup', _onCropMouseUp);
  c.removeEventListener('wheel', _onCropWheel);
  c.removeEventListener('touchstart', _onCropTouchStart);
  c.removeEventListener('touchmove', _onCropTouchMove);
  c.removeEventListener('touchend', _onCropTouchEnd);
}

// ── Pronouns dropdown ─────────────────────────────────────────────────────────

function onPronounsChange(select) {
  var wrap = document.getElementById('pronouns-custom-wrap');
  var hidden = document.getElementById('pronouns-hidden');
  if (select.value === '_custom') {
    wrap.style.display = 'block';
    hidden.value = document.getElementById('pronouns-custom-input').value;
    document.getElementById('pronouns-custom-input').focus();
  } else {
    wrap.style.display = 'none';
    hidden.value = select.value;
  }
}

function _initPronouns() {
  var select = document.getElementById('pronouns-preset');
  var hidden = document.getElementById('pronouns-hidden');
  var customWrap = document.getElementById('pronouns-custom-wrap');
  var customInput = document.getElementById('pronouns-custom-input');
  if (!select || !hidden) return;

  var presets = ['he/him','she/her','they/them','he/they','she/they','it/its','xe/xem','ve/ver','any'];
  var current = hidden.value;

  if (!current) {
    customWrap.style.display = 'none';
    return;
  }
  if (presets.indexOf(current) !== -1) {
    select.value = current;
    customWrap.style.display = 'none';
  } else {
    select.value = '_custom';
    customInput.value = current;
    customWrap.style.display = 'block';
  }
}

// ── Bio char counter ──────────────────────────────────────────────────────────

function _initBioCounter() {
  var bio = document.getElementById('settings-bio');
  var counter = document.getElementById('bio-char-count');
  if (!bio || !counter) return;
  var limit = parseInt(bio.getAttribute('maxlength'), 10) || 160;

  function update() {
    var remaining = limit - bio.value.length;
    counter.textContent = remaining;
    counter.style.color = remaining < 20
      ? (remaining <= 0 ? 'var(--danger)' : '#f59e0b')
      : 'var(--text-muted)';
  }
  bio.addEventListener('input', update);
  update();
}

// ── Location detect ───────────────────────────────────────────────────────────

function detectLocation(btn) {
  if (!navigator.geolocation) {
    if (window.toast) window.toast('Geolocation not supported.');
    return;
  }
  var origHTML = btn.innerHTML;
  btn.disabled = true;
  btn.textContent = 'Detecting…';

  navigator.geolocation.getCurrentPosition(
    function(pos) {
      var lat = pos.coords.latitude.toFixed(5);
      var lon = pos.coords.longitude.toFixed(5);
      fetch('https://nominatim.openstreetmap.org/reverse?format=json&lat=' + lat + '&lon=' + lon, {
        headers: { 'Accept-Language': 'en' }
      })
        .then(function(r) { return r.json(); })
        .then(function(data) {
          var addr = data.address || {};
          var city = addr.city || addr.town || addr.village || addr.county || '';
          var country = addr.country || '';
          var loc = city && country ? city + ', ' + country : (city || country || data.display_name || '');
          var input = document.getElementById('location-input');
          if (input) input.value = loc;
          var cc = document.getElementById('country-code-hidden');
          if (cc && addr.country_code) cc.value = addr.country_code.toUpperCase();
          btn.disabled = false;
          btn.innerHTML = origHTML;
        })
        .catch(function() {
          btn.disabled = false;
          btn.innerHTML = origHTML;
          if (window.toast) window.toast('Could not determine location name.');
        });
    },
    function(err) {
      btn.disabled = false;
      btn.innerHTML = origHTML;
      var msg = err.code === 1 ? 'Location permission denied.' : 'Could not get location.';
      if (window.toast) window.toast(msg);
    },
    { timeout: 8000, maximumAge: 60000 }
  );
}

// ── Theme live preview ────────────────────────────────────────────────────────
// applyThemePreview is defined in f33d3r.js (shared runtime) so it is available
// on every page, including when the edit_profile_modal is opened outside /settings.

// ── Dark mode checkbox ────────────────────────────────────────────────────────

function _initDarkModeCheckbox() {
  var checkbox = document.getElementById('mode-toggle-checkbox');
  if (!checkbox) return;
  checkbox.checked = document.documentElement.dataset.mode === 'dark';
  checkbox.addEventListener('change', function() {
    if (window.toggleDarkMode) {
      window.toggleDarkMode();
    } else {
      var mode = this.checked ? 'dark' : 'light';
      document.documentElement.dataset.mode = mode;
      localStorage.setItem('f33d3r_mode', mode);
    }
    // Re-sync after toggle (toggleDarkMode flips the value)
    var isDark = document.documentElement.dataset.mode === 'dark';
    checkbox.checked = isDark;
  });
}

// ── Website auto-https ────────────────────────────────────────────────────────

function _initWebsitePrefix() {
  var form = document.getElementById('settings-form');
  if (!form) return;
  form.addEventListener('submit', function() {
    var input = document.getElementById('website-input');
    if (!input) return;
    var v = input.value.trim();
    if (v && !v.startsWith('http://') && !v.startsWith('https://')) {
      input.value = 'https://' + v;
    }
  });
}

// ── Social link display (strip prefix on load) ────────────────────────────────

function _initSocialLinks() {
  document.querySelectorAll('.social-link-input').forEach(function(input) {
    var prefix = input.dataset.prefix;
    if (!prefix) return;
    if (input.value && input.value.startsWith(prefix)) {
      input.value = input.value.slice(prefix.length);
    }
  });
}

// ── Password change ───────────────────────────────────────────────────────────

function changePassword() {
  var current = document.getElementById('pw-current');
  var nw = document.getElementById('pw-new');
  var confirm = document.getElementById('pw-confirm');
  var result = document.getElementById('pw-result');
  if (!nw || !confirm || !result) return;

  if (nw.value !== confirm.value) {
    result.innerHTML = '<span style="color:var(--danger);font-size:13px">Passwords do not match.</span>';
    return;
  }
  if (nw.value.length < 8) {
    result.innerHTML = '<span style="color:var(--danger);font-size:13px">Password must be at least 8 characters.</span>';
    return;
  }

  result.innerHTML = '<span style="color:var(--text-muted);font-size:13px">Saving…</span>';

  var body = new URLSearchParams();
  if (current) body.append('current_password', current.value);
  body.append('new_password', nw.value);
  body.append('confirm_password', confirm.value);

  fetch('/api/settings/password', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: body.toString()
  })
    .then(function(r) { return r.text(); })
    .then(function(html) {
      result.innerHTML = html;
      if (html.indexOf('updated') !== -1 || html.indexOf('✓') !== -1) {
        if (current) current.value = '';
        nw.value = '';
        confirm.value = '';
      }
    })
    .catch(function() {
      result.innerHTML = '<span style="color:var(--danger);font-size:13px">Request failed. Try again.</span>';
    });
}

// ── Data export ───────────────────────────────────────────────────────────────

function exportData() {
  fetch('/api/data/export', { method: 'POST' })
    .then(function(r) {
      if (r.ok) {
        if (window.toast) window.toast('Export started — you\'ll receive a link when it\'s ready.');
      } else {
        if (window.toast) window.toast('Export failed. Try again later.');
      }
    })
    .catch(function() {
      if (window.toast) window.toast('Export failed. Try again later.');
    });
}

// ── Settings nav active state ─────────────────────────────────────────────────

function settingsNavActivate(el) {
  document.querySelectorAll('.settings-nav-item').forEach(function(i){ i.classList.remove('active'); });
  el.classList.add('active');
}

// ── Mobile panel toggle ───────────────────────────────────────────────────────
// On mobile (≤767px): tapping a nav item shows the content panel full-screen.
// The back button calls settingsMobBack() to return to the nav list.

function _settingsShell() { return document.getElementById('main-col'); }

window.settingsMobBack = function() {
  var shell = _settingsShell();
  if (shell) shell.classList.remove('settings-shell--panel-open');
};

function _settingsOpenPanel() {
  if (window.innerWidth > 767) return;
  var shell = _settingsShell();
  if (shell) shell.classList.add('settings-shell--panel-open');
}

function _initSettingsMobileNav() {
  // Wire nav item clicks to open the panel on mobile
  document.querySelectorAll('.settings-nav-item').forEach(function(el) {
    el.addEventListener('click', function() {
      settingsNavActivate(el);
      _settingsOpenPanel();
    });
  });
  // When HTMX swaps the settings panel (e.g. after back-button HTMX restore),
  // ensure mobile panel state is consistent with whether a section is active.
  document.body.addEventListener('htmx:afterSwap', function(e) {
    if (e.detail && e.detail.target && e.detail.target.id === 'settings-panel') {
      if (e.detail.target.children.length > 0) {
        _settingsOpenPanel();
      }
    }
  });
  // On page load with a section in the URL, open the panel on mobile
  if (window.location.pathname.match(/^\/settings\/.+/)) {
    _settingsOpenPanel();
  }
}

// ── Init ──────────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', function() {
  _initPronouns();
  _initBioCounter();
  _initDarkModeCheckbox();
  _initWebsitePrefix();
  _initSocialLinks();
  _initSettingsMobileNav();
});
