// kyc.js — camera capture and posting for the verification flow. Registered as
// a Shell page: the mount runs on every arrival of the /kyc Playground and its
// teardown releases any camera the flow still holds.
//
// This file decides nothing. The server renders every step as a Fragment
// (facet:f33d3r:kyc:<account>:flow and :liveness), chooses the liveness prompts,
// and judges every image; htmx posts the forms and swaps the answers in. What
// happens here: a camera is opened, a frame is put into the hidden field the
// server named, and when the server-issued face scan has been run the capture
// form is told to submit.
F33D3R.page('kyc', function (root) {
  var streams = [];

  function stopAll() {
    streams.forEach(function (s) { s.getTracks().forEach(function (t) { t.stop(); }); });
    streams = [];
  }
  function stop(stream) {
    if (!stream) return;
    stream.getTracks().forEach(function (t) { t.stop(); });
    streams = streams.filter(function (s) { return s !== stream; });
  }

  async function openCamera(video, facing) {
    var stream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: { ideal: facing }, width: { ideal: 1280 }, height: { ideal: 960 } },
      audio: false
    });
    streams.push(stream);
    video.srcObject = stream;
    await video.play();
    return stream;
  }

  // A JPEG of what the element shows, no larger than max pixels on its long side.
  function jpeg(source, w, h, max) {
    var scale = Math.min(1, max / Math.max(w, h));
    var canvas = document.createElement('canvas');
    canvas.width = Math.round(w * scale);
    canvas.height = Math.round(h * scale);
    canvas.getContext('2d').drawImage(source, 0, 0, canvas.width, canvas.height);
    return canvas.toDataURL('image/jpeg', 0.9);
  }
  function frameOf(video) { return jpeg(video, video.videoWidth || 1280, video.videoHeight || 960, 1600); }

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  // ── Document sides ─────────────────────────────────────────────────────────
  function slotOf(el) { return el.closest('[data-kyc-capture]'); }
  function fill(slot, dataURL) {
    var field = slot.querySelector('[data-kyc-slot]');
    var preview = slot.querySelector('[data-kyc-preview]');
    var video = slot.querySelector('[data-kyc-video]');
    var state = slot.querySelector('[data-kyc-state]');
    field.value = dataURL;
    preview.src = dataURL;
    preview.hidden = false;
    video.hidden = true;
    if (state) state.textContent = 'Captured. Capture again to replace it.';
  }

  async function onClick(e) {
    var btn = e.target.closest('[data-kyc-action]');
    if (!btn || !root.contains(btn)) return;
    var slot = slotOf(btn);
    if (!slot) return;
    var video = slot.querySelector('[data-kyc-video]');
    var state = slot.querySelector('[data-kyc-state]');
    var shoot = slot.querySelector('[data-kyc-action="shoot"]');
    if (btn.dataset.kycAction === 'camera') {
      try {
        video.hidden = false;
        slot.querySelector('[data-kyc-preview]').hidden = true;
        slot._kycStream = await openCamera(video, 'environment');
        shoot.disabled = false;
        if (state) state.textContent = 'Line the document up, then capture.';
      } catch (err) {
        if (state) state.textContent = 'The camera could not be opened. Upload a photo instead.';
      }
    } else if (btn.dataset.kycAction === 'shoot') {
      fill(slot, frameOf(video));
      stop(slot._kycStream);
      slot._kycStream = null;
      shoot.disabled = true;
    }
  }

  function onChange(e) {
    var input = e.target.closest('[data-kyc-action="file"]');
    if (!input || !root.contains(input) || !input.files || !input.files[0]) return;
    var slot = slotOf(input);
    var img = new Image();
    var url = URL.createObjectURL(input.files[0]);
    img.onload = function () {
      fill(slot, jpeg(img, img.naturalWidth, img.naturalHeight, 2000));
      URL.revokeObjectURL(url);
    };
    img.onerror = function () {
      var state = slot.querySelector('[data-kyc-state]');
      if (state) state.textContent = 'That file could not be opened as an image.';
      URL.revokeObjectURL(url);
    };
    img.src = url;
  }

  // ── The server-issued face scan ────────────────────────────────────────────
  async function runLiveness(box) {
    var form = box.closest('form');
    var video = box.querySelector('[data-kyc-video]');
    var state = box.querySelector('[data-kyc-state]');
    var stream;
    try {
      stream = await openCamera(video, 'user');
    } catch (err) {
      if (state) state.textContent = 'The camera could not be opened. Allow camera access and reload the page.';
      return;
    }
    // The selfie: frontal, neutral, taken before any prompt. Every frame is
    // drawn from the camera image itself, never the mirrored preview.
    if (state) state.textContent = 'Look straight at the camera with a neutral expression.';
    await sleep(2000);
    form.querySelector('[data-kyc-slot="face_image"]').value = frameOf(video);
    var frames = [];
    var prompts = box.querySelectorAll('[data-kyc-challenge]');
    for (var i = 0; i < prompts.length; i++) {
      prompts[i].classList.add('kyc-challenge--active');
      if (state) state.textContent = prompts[i].textContent.trim();
      await sleep(1600);
      frames.push(frameOf(video));
      await sleep(1400);
      prompts[i].classList.remove('kyc-challenge--active');
      prompts[i].classList.add('kyc-challenge--done');
    }
    stop(stream);
    form.querySelector('[data-kyc-slot="liveness_challenge_frames"]').value = JSON.stringify(frames);
    if (state) state.textContent = 'Sending…';
    htmx.trigger(form, 'kyc-submit');
  }

  // After any swap, a face-scan Facet the server has just made ready is run
  // once. The Facet is looked up rather than read off the event: an outerHTML
  // swap reports the element it replaced.
  function onSettle() {
    var box = root.querySelector('[data-kyc-liveness="ready"]:not([data-kyc-running])');
    if (!box) return;
    box.setAttribute('data-kyc-running', '');
    runLiveness(box);
  }

  // A new flow Fragment replaces the old one: its cameras go with it.
  function onBeforeSwap(e) {
    if (e.target && e.target.id === 'kyc-flow') stopAll();
  }

  root.addEventListener('click', onClick);
  root.addEventListener('change', onChange);
  document.body.addEventListener('htmx:afterSettle', onSettle);
  document.body.addEventListener('htmx:beforeSwap', onBeforeSwap);

  return function teardown() {
    stopAll();
    root.removeEventListener('click', onClick);
    root.removeEventListener('change', onChange);
    document.body.removeEventListener('htmx:afterSettle', onSettle);
    document.body.removeEventListener('htmx:beforeSwap', onBeforeSwap);
  };
});
