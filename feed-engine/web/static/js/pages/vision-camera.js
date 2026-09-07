/**
 * vision-camera.js — the Vision camera
 *
 * Lifecycle:
 *   page mount       → startCamera('environment')
 *   shutter click    → capture() → onCapture(blob) → show preview
 *   send click       → sendVision(blob, caption) → POST /visions → confirmation Fragment
 *   retake click     → hide preview, show camera-state, restart camera
 *   page teardown    → camera stream released
 *
 * JS contract: LISTEN / LOCATE / PATCH / EMIT only.
 * No DOM layout construction. No client-side state libraries.
 *
 * Registered as a Shell page: executed once per session, the mount runs on
 * every arrival of the camera Playground and returns the teardown that stops
 * the camera when the page leaves.
 */
F33D3R.page('vision-camera', function (root) {
  'use strict';

  // ── LOCATE ────────────────────────────────────────────────────────────────
  var videoEl      = document.getElementById('vc-video');
  var cameraState  = document.getElementById('vc-camera-state');
  var preview      = document.getElementById('vc-preview');
  var previewImg   = document.getElementById('vc-preview-img');
  var shutterBtn   = document.getElementById('vc-shutter');
  var flipBtn      = document.getElementById('vc-flip');
  var flashBtn     = document.getElementById('vc-flash');
  var lutBtn       = document.getElementById('vc-lut');
  var closeBtn     = document.getElementById('vc-close');
  var retakeBtn    = document.getElementById('vc-retake');
  var sendBtn      = document.getElementById('vc-send');
  var captionInput = document.getElementById('vc-caption');
  var sendingEl    = document.getElementById('vc-sending');

  if (!videoEl || !shutterBtn || !flipBtn || !flashBtn || !lutBtn || !closeBtn || !retakeBtn || !sendBtn || !captionInput || !sendingEl || !preview || !previewImg) {
    return;
  }

  var currentStream  = null;
  var currentTrack   = null;
  var currentFacing  = 'environment';
  var currentLUT     = 'raw';
  var capturedBlob   = null;
  var torchOn        = false;

  var LUT_NAMES = ['raw', 'film', 'cool', 'fade'];

  var LUTS = {
    raw:  null,
    film: { warm: 15, contrast: 1.08, lift: 8 },
    cool: { warm: -12, contrast: 1.05, saturation: 0.85 },
    fade: { lift: 20, contrast: 0.90, saturation: 0.90 }
  };

  var LUT_CSS = {
    raw:  '',
    film: 'sepia(0.15) contrast(1.08) brightness(1.02)',
    cool: 'saturate(0.85) hue-rotate(15deg) contrast(1.05)',
    fade: 'contrast(0.9) saturate(0.9) brightness(1.08)'
  };

  function stopStream() {
    if (currentStream) {
      currentStream.getTracks().forEach(function (t) { t.stop(); });
      currentStream = null;
      currentTrack  = null;
    }
  }

  // ── Camera init ───────────────────────────────────────────────────────────
  async function startCamera(facing) {
    stopStream();
    try {
      currentStream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: facing, width: { ideal: 1920 }, height: { ideal: 1080 } },
        audio: false
      });
    } catch (err) {
      console.error('[vision-camera] getUserMedia failed:', err);
      if (typeof window.toast === 'function') {
        window.toast('Camera access denied. Please allow camera access and try again.', 'error');
      }
      return;
    }
    // The page left while the camera was being opened: release it.
    if (!videoEl.isConnected) { stopStream(); return; }

    var track = currentStream.getVideoTracks()[0];
    currentTrack = track;

    // Apply continuous focus + exposure when supported.
    try {
      var caps = track.getCapabilities ? track.getCapabilities() : {};
      var adv = [];
      if (caps.focusMode && caps.focusMode.indexOf('continuous') !== -1) {
        adv.push({ focusMode: 'continuous' });
      }
      if (caps.exposureMode && caps.exposureMode.indexOf('continuous') !== -1) {
        adv.push({ exposureMode: 'continuous' });
      }
      if (adv.length) {
        await track.applyConstraints({ advanced: adv });
      }

      // Torch: only show flash button if torch is a supported capability.
      if (caps.torch === true) {
        flashBtn.style.visibility = '';
        torchOn = false;
        updateFlashIcons();
      } else {
        flashBtn.style.visibility = 'hidden';
      }
    } catch (e) {
      // Advanced constraints not supported — harmless.
      flashBtn.style.visibility = 'hidden';
    }

    videoEl.srcObject = currentStream;
    videoEl.style.filter = LUT_CSS[currentLUT] || '';
    // Mirror preview for front camera so it feels like a mirror; rear camera shows world as-is.
    var isFront = facing === 'user';
    videoEl.classList.toggle('camera-preview--front', isFront);
    videoEl.classList.toggle('camera-preview--rear', !isFront);
  }

  function updateFlashIcons() {
    var off = flashBtn.querySelector('.vc-flash-icon--off');
    var on  = flashBtn.querySelector('.vc-flash-icon--on');
    if (off) off.style.display = torchOn ? 'none' : '';
    if (on)  on.style.display  = torchOn ? '' : 'none';
  }

  // ── LUT pixel processing ──────────────────────────────────────────────────
  function applyLUT(ctx, w, h, lut) {
    if (!lut) return;
    var img = ctx.getImageData(0, 0, w, h);
    var d = img.data;
    for (var i = 0; i < d.length; i += 4) {
      var r = d[i], g = d[i + 1], b = d[i + 2];

      // Warmth shift
      if (lut.warm > 0) {
        r = Math.min(255, r + lut.warm);
        b = Math.max(0, b - lut.warm * 0.5);
      } else if (lut.warm < 0) {
        b = Math.min(255, b - lut.warm);
        r = Math.max(0, r + lut.warm * 0.5);
      }

      // Lift (raised blacks for fade/matte look)
      if (lut.lift) {
        r = r + (lut.lift * (1 - r / 255));
        g = g + (lut.lift * (1 - g / 255));
        b = b + (lut.lift * (1 - b / 255));
      }

      // Contrast
      if (lut.contrast) {
        var f = lut.contrast;
        r = Math.min(255, Math.max(0, (r - 128) * f + 128));
        g = Math.min(255, Math.max(0, (g - 128) * f + 128));
        b = Math.min(255, Math.max(0, (b - 128) * f + 128));
      }

      // Saturation
      if (lut.saturation != null) {
        var avg = (r + g + b) / 3;
        r = Math.min(255, avg + (r - avg) * lut.saturation);
        g = Math.min(255, avg + (g - avg) * lut.saturation);
        b = Math.min(255, avg + (b - avg) * lut.saturation);
      }

      d[i]     = r | 0;
      d[i + 1] = g | 0;
      d[i + 2] = b | 0;
    }
    ctx.putImageData(img, 0, 0);
  }

  // ── Capture: bakes vignette + rounded corners + LUT into JPEG pixels ─────
  function capture() {
    var w = videoEl.videoWidth;
    var h = videoEl.videoHeight;
    if (!w || !h) {
      console.warn('[vision-camera] video not ready yet');
      return;
    }

    var canvas = document.createElement('canvas');
    canvas.width  = w;
    canvas.height = h;
    var ctx = canvas.getContext('2d');

    // 1. Draw the current video frame.
    ctx.drawImage(videoEl, 0, 0);

    // 2. Apply LUT to pixel data.
    applyLUT(ctx, w, h, LUTS[currentLUT]);

    // 3. Bake vignette (radial gradient — black at edges, transparent in center).
    var grad = ctx.createRadialGradient(
      w / 2, h / 2, Math.min(w, h) * 0.3,
      w / 2, h / 2, Math.max(w, h) * 0.75
    );
    grad.addColorStop(0, 'rgba(0,0,0,0)');
    grad.addColorStop(1, 'rgba(0,0,0,0.55)');
    ctx.fillStyle = grad;
    ctx.fillRect(0, 0, w, h);

    // 4. Bake rounded corners into a JPEG with black corners.
    //    JPEG has no alpha channel — transparent pixels composite to white by default.
    //    Fill the offscreen canvas black FIRST (before clipping) so corners stay black.
    var offscreen = document.createElement('canvas');
    offscreen.width  = w;
    offscreen.height = h;
    var octx = offscreen.getContext('2d');
    var radius = Math.min(w, h) * 0.15; // 15% — dramatic squircle matching Vision aesthetic

    // Black background fills the corners when JPEG collapses the alpha channel.
    octx.fillStyle = '#000';
    octx.fillRect(0, 0, w, h);

    // Clip to squircle and draw the vignette-processed frame inside it.
    octx.save();
    octx.beginPath();
    if (octx.roundRect) {
      octx.roundRect(0, 0, w, h, radius);
    } else {
      // Fallback for browsers without roundRect.
      octx.moveTo(radius, 0);
      octx.lineTo(w - radius, 0);
      octx.arcTo(w, 0, w, radius, radius);
      octx.lineTo(w, h - radius);
      octx.arcTo(w, h, w - radius, h, radius);
      octx.lineTo(radius, h);
      octx.arcTo(0, h, 0, h - radius, radius);
      octx.lineTo(0, radius);
      octx.arcTo(0, 0, radius, 0, radius);
    }
    octx.closePath();
    octx.clip();
    octx.drawImage(canvas, 0, 0);
    octx.restore();

    offscreen.toBlob(function (blob) { onCapture(blob); }, 'image/jpeg', 0.92);
  }

  function onCapture(blob) {
    if (!blob) return;
    capturedBlob = blob;

    // Show preview state; hide the entire camera-state wrapper.
    var objectURL = URL.createObjectURL(blob);
    previewImg.src = objectURL;
    document.getElementById('vc-camera-state').style.display = 'none';
    preview.style.display = '';
    captionInput.focus();

    // Stop the camera stream to save battery while in preview.
    stopStream();
  }

  // ── Send flow ─────────────────────────────────────────────────────────────
  // LOCATE the server's rejection wording inside the Fragment it rendered.
  function visionRefusalReason(fragment) {
    try {
      var doc = new DOMParser().parseFromString(fragment, 'text/html');
      var el = doc.querySelector('.vision-compose__error');
      return el ? el.textContent.trim() : '';
    } catch (e) {
      return '';
    }
  }

  // One request. The frame and its caption go to the Vision compose path, which
  // stores the object under the expirable Vision media class, writes the Vision
  // and answers with the rendered confirmation Fragment. A Vision is not a Work
  // and is never signed as one.
  async function sendVision(blob, caption) {
    sendBtn.disabled = true;
    sendingEl.style.display = '';

    try {
      var fd = new FormData();
      fd.append('content_type', 'image');
      fd.append('source', 'camera');
      fd.append('body', caption || '');
      fd.append('media', blob, 'vision.jpg');

      var res = await fetch('/visions', { method: 'POST', body: fd });
      if (res.redirected) {
        // The session ended mid-capture and the write was sent to the login
        // gate. The login page is a different Shell: a document load is the
        // only honest answer here.
        window.location.href = res.url;
        return;
      }
      var fragment = await res.text();
      if (!res.ok) {
        // The server rendered its own reason for refusing. LOCATE it and show
        // those words — the browser does not author the rejection.
        var reason = visionRefusalReason(fragment);
        console.error('[vision-camera] vision refused:', res.status, reason);
        sendingEl.style.display = 'none';
        sendBtn.disabled = false;
        if (typeof window.toast === 'function') {
          window.toast(reason || 'Failed to post Vision. Please try again.', 'error');
        }
        return;
      }

      // PATCH: the server rendered the confirmation; this only places it.
      if (previewImg.src) {
        URL.revokeObjectURL(previewImg.src);
      }
      capturedBlob = null;
      preview.innerHTML = fragment;
      if (window.htmx && typeof window.htmx.process === 'function') {
        window.htmx.process(preview);
      }

    } catch (err) {
      console.error('[vision-camera] send failed:', err);
      sendingEl.style.display = 'none';
      sendBtn.disabled = false;
      if (typeof window.toast === 'function') {
        window.toast('Failed to post Vision. Please try again.', 'error');
      }
    }
  }

  // ── LISTEN — wire all interactions ────────────────────────────────────────
  // Boot camera immediately.
  startCamera(currentFacing);

  // Shutter: capture current frame.
  shutterBtn.addEventListener('click', function () {
    capture();
  });

  // Flip: toggle front/rear camera.
  flipBtn.addEventListener('click', function () {
    currentFacing = (currentFacing === 'environment') ? 'user' : 'environment';
    startCamera(currentFacing);
  });

  // Flash: toggle torch (only wired if the button is visible — i.e. torch is supported).
  flashBtn.addEventListener('click', function () {
    if (!currentTrack) return;
    torchOn = !torchOn;
    try {
      currentTrack.applyConstraints({ advanced: [{ torch: torchOn }] });
    } catch (e) {
      torchOn = !torchOn; // revert
    }
    updateFlashIcons();
  });

  // LUT toggle: cycle through Raw → Film → Cool → Fade → Raw.
  lutBtn.addEventListener('click', function () {
    var idx = LUT_NAMES.indexOf(currentLUT);
    currentLUT = LUT_NAMES[(idx + 1) % LUT_NAMES.length];
    lutBtn.textContent = currentLUT.charAt(0).toUpperCase() + currentLUT.slice(1);
    // Apply live CSS approximation on the video element for immediate feedback.
    videoEl.style.filter = LUT_CSS[currentLUT] || '';
  });

  // Close / back: avatar button in header navigates back — a Playground
  // navigation, never a document load.
  closeBtn.addEventListener('click', function () {
    if (window.history.length > 1) {
      window.history.back();
    } else {
      F33D3R.visit('/');
    }
  });

  // Retake: discard captured image, restore camera-state wrapper, restart camera.
  retakeBtn.addEventListener('click', function () {
    capturedBlob = null;
    previewImg.src = '';
    preview.style.display = 'none';
    document.getElementById('vc-camera-state').style.display = '';
    startCamera(currentFacing);
  });

  // Send: upload + POST /events.
  sendBtn.addEventListener('click', function () {
    if (!capturedBlob) return;
    sendVision(capturedBlob, captionInput.value.trim());
  });

  // Allow Enter in caption to submit.
  captionInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      sendBtn.click();
    }
  });

  // The page leaves: the camera is released with it.
  return function teardown() {
    stopStream();
    if (previewImg.src && previewImg.src.indexOf('blob:') === 0) {
      URL.revokeObjectURL(previewImg.src);
    }
  };
});
