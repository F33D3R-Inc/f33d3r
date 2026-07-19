// ── Creator mode detection ─────────────────────────────────────────────────────
var isCreatorMode = (document.getElementById('kyc-flow-root') || {}).dataset?.kycMode === 'creator';

// ── State ─────────────────────────────────────────────────────────────────────
var KYC = {
  docImageB64:       null,
  faceImageB64:      null,
  challengeFrames:   [],
  livenessSessionId: null,
  challenges:        [],
  currentChallenge:  0,
  docStream:         null,
  faceStream:        null,
};

// ── Step navigation ────────────────────────────────────────────────────────────
function kycGo(step) {
  document.querySelectorAll('.kyc-step').forEach(function(el) { el.classList.add('hidden'); });

  if (step !== 'doc_capture' && KYC.docStream) {
    KYC.docStream.getTracks().forEach(function(t) { t.stop(); });
    KYC.docStream = null;
  }
  if (step !== 'liveness' && KYC.faceStream) {
    KYC.faceStream.getTracks().forEach(function(t) { t.stop(); });
    KYC.faceStream = null;
  }

  var el = document.getElementById('kyc-step-' + step);
  if (el) el.classList.remove('hidden');

  // Update progress dots
  // Creator mode has 5 dots: doc_capture, doc_confirm, liveness, agreement, result
  // Standard mode has 4 dots: doc_capture, doc_confirm, liveness, result
  var stepOrder = isCreatorMode
    ? ['doc_capture', 'doc_confirm', 'liveness', 'agreement', 'result']
    : ['doc_capture', 'doc_confirm', 'liveness', 'result'];
  var idx = stepOrder.indexOf(step); // -1 for intro/processing
  var dotCount = isCreatorMode ? 5 : 4;
  for (var i = 1; i <= dotCount; i++) {
    var dot = document.getElementById('kyc-dot-' + i);
    if (dot) dot.classList.toggle('kyc-dot--active', i <= idx + 1);
  }

  if (step === 'doc_capture') _kycInitDocCamera();
  if (step === 'liveness')    _kycInitLiveness();
}

// ── Document camera ────────────────────────────────────────────────────────────
async function _kycInitDocCamera() {
  var video     = document.getElementById('kyc-doc-video');
  var statusEl  = document.getElementById('kyc-doc-cam-status');
  var captureBtn = document.getElementById('kyc-doc-capture-btn');

  try {
    var stream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: { ideal: 'environment' }, width: { ideal: 1280 }, height: { ideal: 720 } }
    });
    KYC.docStream = stream;
    video.srcObject = stream;
    await video.play();
    // Do NOT mirror doc camera — user must align ID as-is.
    // Detect actual facingMode in case the device fell back to front camera.
    var track = stream.getVideoTracks()[0];
    var settings = track ? track.getSettings() : {};
    var isFront = settings.facingMode === 'user';
    video.classList.toggle('camera-preview--front', isFront);
    video.classList.toggle('camera-preview--rear', !isFront);
    if (statusEl) statusEl.textContent = '';
    if (captureBtn) captureBtn.disabled = false;
  } catch (e) {
    if (statusEl) statusEl.textContent = 'Camera unavailable — use Upload File';
    console.warn('[kyc] doc camera error:', e);
  }
}

function kycCaptureDoc() {
  var video  = document.getElementById('kyc-doc-video');
  var canvas = document.getElementById('kyc-doc-canvas');
  canvas.width  = video.videoWidth  || 1280;
  canvas.height = video.videoHeight || 720;
  var ctx = canvas.getContext('2d');
  ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
  KYC.docImageB64 = canvas.toDataURL('image/jpeg', 0.92);
  _kycShowDocConfirm();
}

function kycDocFromFile(input) {
  var file = input.files[0];
  if (!file) return;
  var reader = new FileReader();
  reader.onload = function(e) {
    KYC.docImageB64 = e.target.result;
    _kycShowDocConfirm();
  };
  reader.readAsDataURL(file);
}

function _kycShowDocConfirm() {
  var preview = document.getElementById('kyc-doc-preview');
  if (preview) preview.src = KYC.docImageB64;
  kycGo('doc_confirm');
}

function kycConfirmDoc() {
  kycGo('liveness');
}

// ── Liveness ───────────────────────────────────────────────────────────────────
async function _kycInitLiveness() {
  var video       = document.getElementById('kyc-face-video');
  var camStatus   = document.getElementById('kyc-face-cam-status');
  var challStatus = document.getElementById('kyc-challenge-status');

  // Start front camera
  try {
    var stream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: 'user', width: { ideal: 640 }, height: { ideal: 480 } }
    });
    KYC.faceStream = stream;
    video.srcObject = stream;
    await video.play();
    // Mirror the liveness preview so the user sees a natural selfie mirror.
    // The CSS class overrides the inline transform already set on the element.
    video.classList.add('camera-preview--front');
    video.classList.remove('camera-preview--rear');
    if (camStatus) camStatus.textContent = '';
  } catch (e) {
    if (camStatus) camStatus.textContent = 'Camera unavailable';
    if (challStatus) challStatus.textContent = 'Allow camera access and reload the page.';
    console.warn('[kyc] face camera error:', e);
    return;
  }

  // Fetch server-side challenge session
  if (challStatus) challStatus.textContent = 'Preparing challenge…';
  try {
    var r = await fetch('/kyc/liveness/challenge', { method: 'POST' });
    if (!r.ok) throw new Error('http ' + r.status);
    var d = await r.json();
    KYC.livenessSessionId = d.session_id;
    KYC.challenges        = d.challenges || ['blink', 'look_up', 'smile'];
  } catch (e) {
    // Dev fallback
    KYC.livenessSessionId = 'dev-session';
    KYC.challenges        = ['blink', 'look_up', 'smile'];
    console.warn('[kyc] challenge fetch failed, using dev fallback:', e);
  }

  // Render challenge progress pills
  var pillsEl = document.getElementById('kyc-challenge-pills');
  if (pillsEl) {
    pillsEl.innerHTML = KYC.challenges.map(function(_, i) {
      return '<div class="kyc-challenge-pill" id="kyc-pill-' + i + '"></div>';
    }).join('');
  }

  KYC.challengeFrames  = [];
  KYC.currentChallenge = 0;

  // Give the user a moment to position their face, then capture baseline selfie
  if (challStatus) challStatus.textContent = 'Position your face in the frame…';
  await _sleep(1800);

  KYC.faceImageB64 = _kycCaptureFaceFrame();
  if (challStatus) challStatus.textContent = '';

  _kycNextChallenge();
}

function _kycCaptureFaceFrame() {
  var video  = document.getElementById('kyc-face-video');
  var canvas = document.getElementById('kyc-face-canvas');
  canvas.width  = video.videoWidth  || 640;
  canvas.height = video.videoHeight || 480;
  var ctx = canvas.getContext('2d');
  // Draw without mirror transform — CSS scaleX(-1) is display-only
  ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
  return canvas.toDataURL('image/jpeg', 0.85);
}

var _CHALLENGE_META = {
  blink:   { icon: '👁️', text: 'Blink',    sub: 'Blink your eyes naturally' },
  look_up: { icon: '⬆️', text: 'Look Up',  sub: 'Tilt your gaze upward' },
  smile:   { icon: '😊', text: 'Smile',    sub: 'Give a natural smile' },
};

async function _kycNextChallenge() {
  var i = KYC.currentChallenge;
  if (i >= KYC.challenges.length) {
    _kycSubmitVerification();
    return;
  }

  var ch   = KYC.challenges[i];
  var meta = _CHALLENGE_META[ch] || { icon: '✓', text: ch, sub: '' };
  var DURATION = 3200; // ms to hold each challenge

  var overlay   = document.getElementById('kyc-challenge-overlay');
  var iconEl    = document.getElementById('kyc-challenge-icon');
  var textEl    = document.getElementById('kyc-challenge-text');
  var subEl     = document.getElementById('kyc-challenge-sub');
  var bar       = document.getElementById('kyc-challenge-timer-bar');
  var flash     = document.getElementById('kyc-challenge-flash');
  var challStatus = document.getElementById('kyc-challenge-status');

  if (iconEl)  iconEl.textContent  = meta.icon;
  if (textEl)  textEl.textContent  = meta.text;
  if (subEl)   subEl.textContent   = meta.sub;
  if (overlay) overlay.style.display = '';

  // Countdown bar
  if (bar) {
    bar.style.transition = 'none';
    bar.style.width = '100%';
    await _sleep(30);
    bar.style.transition = 'width ' + DURATION + 'ms linear';
    bar.style.width = '0%';
  }

  // Capture frame at the halfway point — user is more likely mid-action
  await _sleep(DURATION / 2);
  var frame = _kycCaptureFaceFrame();
  KYC.challengeFrames.push(frame);

  // Visual feedback flash
  if (flash) { flash.style.display = 'block'; }
  await _sleep(200);
  if (flash) { flash.style.display = 'none'; }

  // Mark pill complete
  var pill = document.getElementById('kyc-pill-' + i);
  if (pill) pill.style.background = 'var(--accent)';

  await _sleep(DURATION / 2);
  if (overlay) overlay.style.display = 'none';

  KYC.currentChallenge++;

  var remaining = KYC.challenges.length - KYC.currentChallenge;
  if (challStatus && remaining > 0) {
    challStatus.textContent = remaining + ' challenge' + (remaining === 1 ? '' : 's') + ' remaining';
  } else if (challStatus) {
    challStatus.textContent = 'Processing…';
  }

  await _sleep(500);
  _kycNextChallenge();
}

// ── Submit ─────────────────────────────────────────────────────────────────────
async function _kycSubmitVerification() {
  if (KYC.faceStream) {
    KYC.faceStream.getTracks().forEach(function(t) { t.stop(); });
    KYC.faceStream = null;
  }

  kycGo('processing');

  var form = new FormData();
  form.append('document_image',           KYC.docImageB64   || '');
  form.append('face_image',               KYC.faceImageB64  || '');
  form.append('liveness_session_id',      KYC.livenessSessionId || '');
  form.append('liveness_challenge_frames', JSON.stringify(KYC.challengeFrames));
  form.append('liveness_passed',           String(KYC.challengeFrames.length));

  try {
    var r   = await fetch('/kyc/submit', { method: 'POST', body: form });
    var html = await r.text();
    var isSuccess = html.indexOf('22c55e') !== -1;

    if (isCreatorMode && isSuccess) {
      // In creator mode, proceed to the agreement step instead of showing reload link.
      kycGo('agreement');
      var agreementEl = document.getElementById('kyc-agreement-content');
      if (agreementEl) {
        agreementEl.innerHTML = '<span style="color:var(--text-secondary);font-size:13px">Loading statement…</span>';
        fetch('/kyc/creator/statement')
          .then(function(sr) { return sr.text(); })
          .then(function(sh) { agreementEl.innerHTML = sh; })
          .catch(function() { agreementEl.innerHTML = '<span style="color:var(--text-secondary);font-size:13px">Could not load statement — scroll to read it on the <a href="/legal/2257" style="color:var(--accent)">2257 page</a>.</span>'; });
      }
    } else {
      kycGo('result');
      var content = document.getElementById('kyc-result-content');
      if (content) content.innerHTML = html;
      // Append a reload link on success
      if (content && isSuccess) {
        content.innerHTML += '<br><a href="/kyc" style="color:var(--accent);font-size:13px;font-weight:500">View verification status →</a>';
      }
    }
  } catch (e) {
    kycGo('result');
    var content = document.getElementById('kyc-result-content');
    if (content) content.innerHTML = '<span style="color:#ef4444;font-size:13px">Submission failed — check your connection and try again.</span>';
  }
}

// ── Age-only MRZ submit (Lane 1 path: /kyc?mode=age) ──────────────────────────
async function kycAgeSubmit() {
  if (!KYC.docImage) {
    kycGo('doc_capture');
    return;
  }
  kycGo('processing');
  var form = new FormData();
  form.append('document_image', KYC.docImage);
  try {
    var r    = await fetch('/kyc/age-verify', { method: 'POST', body: form });
    var html = await r.text();
    kycGo('result');
    var content = document.getElementById('kyc-result-content');
    if (content) {
      content.innerHTML = html;
      if (html.indexOf('22c55e') !== -1) {
        content.innerHTML += '<br><a href="/" style="color:var(--accent);font-size:13px;font-weight:500">Back to feed →</a>';
      }
    }
  } catch (e) {
    kycGo('result');
    var content = document.getElementById('kyc-result-content');
    if (content) content.innerHTML = '<span style="color:#ef4444;font-size:13px">Submission failed — check your connection and try again.</span>';
  }
}

// ── Agreement toggle ───────────────────────────────────────────────────────────
function kycAgreementToggle(cb) {
  var btn = document.getElementById('kyc-activate-btn');
  if (btn) btn.disabled = !cb.checked;
}

// ── Helpers ────────────────────────────────────────────────────────────────────
function _sleep(ms) { return new Promise(function(r) { setTimeout(r, ms); }); }

// ── Init ───────────────────────────────────────────────────────────────────────
document.addEventListener('DOMContentLoaded', function() {
  var flow = document.getElementById('kyc-flow-root');
  if (!flow) return;
  // Re-read creator mode after DOM is ready (dataset is available now).
  isCreatorMode = flow.dataset && flow.dataset.kycMode === 'creator';
  kycGo('intro');
});
