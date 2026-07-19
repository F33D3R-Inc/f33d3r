/**
 * vision-camera.js — Visions ephemeral camera
 *
 * Lifecycle:
 *   DOMContentLoaded → startCamera('environment')
 *   shutter click    → capture() → onCapture(blob) → show preview
 *   send click       → sendVision(blob, caption) → POST /events + redirect
 *   retake click     → hide preview, show camera-state, restart camera
 *
 * JS contract: LISTEN / LOCATE / PATCH / EMIT only.
 * No DOM layout construction. No client-side state libraries.
 */
(function () {
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

  // ── Camera init ───────────────────────────────────────────────────────────
  async function startCamera(facing) {
    if (currentStream) {
      currentStream.getTracks().forEach(function (t) { t.stop(); });
    }
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
    if (currentStream) {
      currentStream.getTracks().forEach(function (t) { t.stop(); });
      currentStream = null;
      currentTrack  = null;
    }
  }

  // ── Send flow ─────────────────────────────────────────────────────────────
  async function sendVision(blob, caption) {
    sendBtn.disabled = true;
    sendingEl.style.display = '';

    try {
      // 1. Upload image to Caeor via the existing media upload endpoint.
      var fd = new FormData();
      fd.append('media', blob, 'vision.jpg');
      var uploadRes = await fetch('/upload/post-media', { method: 'POST', body: fd });
      if (!uploadRes.ok) {
        throw new Error('Upload failed: ' + uploadRes.status);
      }
      var uploadData = await uploadRes.json();
      var mediaURL = uploadData.url || '';
      if (!mediaURL) {
        throw new Error('No URL in upload response');
      }

      // 2. Malkuth-sign the work as kind='vision'. The server auto-sets expires_at=NOW()+24h
      //    when kind='vision' is present in the InsertWork path.
      if (!window.Malkuth || !window._malkuthReady) {
        throw new Error('Malkuth not loaded — please reload the page');
      }
      var _mk = await window._malkuthReady;
      if (!_mk || !_mk.hasKey) {
        throw new Error('Signing key unavailable — please reload the page');
      }
      var envelope = await window.Malkuth.buildSignedWork('post', caption || '', 'vision', [mediaURL], null, {});

      // 3. POST /events with the signed envelope (D-070 mutation lane).
      var eventsRes = await fetch('/events', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(envelope)
      });
      if (!eventsRes.ok) {
        throw new Error('Vision post failed: ' + eventsRes.status);
      }

      window.location.href = '/';

    } catch (err) {
      console.error('[vision-camera] send failed:', err);
      sendingEl.style.display = 'none';
      sendBtn.disabled = false;
      if (typeof window.toast === 'function') {
        window.toast('Failed to share vision. Please try again.', 'error');
      }
    }
  }

  // ── LISTEN — wire all interactions ────────────────────────────────────────
  document.addEventListener('DOMContentLoaded', function () {
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

    // Close / back: avatar button in header navigates back.
    closeBtn.addEventListener('click', function () {
      if (window.history.length > 1) {
        window.history.back();
      } else {
        window.location.href = '/';
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
  });
})();
