// Server values from <meta> tags — no template injection in JS
const SESSION_ID    = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
const MY_PIAL       = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE     = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';

// ── Mode toggle ───────────────────────────────────────────────────────────────
function setMusicMode(mode) {
  document.getElementById('music-listen').classList.toggle('hidden', mode !== 'listen');
  document.getElementById('music-artist').classList.toggle('hidden', mode !== 'artist');
  document.getElementById('mode-listen-btn').classList.toggle('active', mode === 'listen');
  document.getElementById('mode-artist-btn').classList.toggle('active', mode === 'artist');
  localStorage.setItem('f33d3r-music-mode', mode);
}
// Restore mode on load
(function() {
  const saved = localStorage.getItem('f33d3r-music-mode');
  if (saved === 'artist') setMusicMode('artist');
})();

// ── Genre filter ──────────────────────────────────────────────────────────────
function filterGenre(btn, genre) {
  document.querySelectorAll('.genre-chip').forEach(c => c.classList.remove('active'));
  btn.classList.add('active');
  htmx.ajax('GET', '/music/tracks?genre=' + genre, {target: '#track-feed', swap: 'innerHTML'});
}

// ── Upload flow ───────────────────────────────────────────────────────────────
let selectedAudioFile = null;
function handleAudioSelect(input) {
  if (input.files && input.files[0]) showUploadForm(input.files[0]);
}
function handleAudioDrop(event) {
  event.preventDefault();
  document.getElementById('upload-drop-zone').classList.remove('drag-over');
  const file = event.dataTransfer.files[0];
  if (file) showUploadForm(file);
}
function showUploadForm(file) {
  selectedAudioFile = file;
  document.getElementById('upload-filename').textContent = file.name;
  document.getElementById('upload-filesize').textContent = (file.size / (1024*1024)).toFixed(1) + ' MB';
  document.getElementById('upload-form-wrap').classList.remove('hidden');
  document.getElementById('upload-drop-zone').classList.add('hidden');
}
function cancelUpload() {
  selectedAudioFile = null;
  document.getElementById('upload-form-wrap').classList.add('hidden');
  document.getElementById('upload-drop-zone').classList.remove('hidden');
  document.getElementById('track-upload-form').reset();
  document.getElementById('cover-preview').style.display = 'none';
  document.getElementById('cover-placeholder').style.display = 'flex';
}
function previewCover(input) {
  if (input.files && input.files[0]) {
    const reader = new FileReader();
    reader.onload = e => {
      const img = document.getElementById('cover-preview');
      img.src = e.target.result;
      img.style.display = 'block';
      document.getElementById('cover-placeholder').style.display = 'none';
    };
    reader.readAsDataURL(input.files[0]);
  }
}
function setPricing(type) {
  document.getElementById('price-free').classList.toggle('active', type === 'free');
  document.getElementById('price-paid').classList.toggle('active', type === 'paid');
}
async function submitTrackUpload() {
  if (!selectedAudioFile) return;
  const form = document.getElementById('track-upload-form');
  const formData = new FormData(form);
  formData.append('audio', selectedAudioFile);

  const btn = document.getElementById('upload-submit-btn');
  btn.disabled = true;
  btn.textContent = 'Uploading…';
  document.getElementById('upload-progress-wrap').classList.remove('hidden');

  try {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/upload/track');
    xhr.upload.onprogress = e => {
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        document.getElementById('upload-progress-bar').style.width = pct + '%';
        document.getElementById('upload-status-text').textContent = 'Uploading… ' + pct + '%';
      }
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        toast('Track uploaded successfully');
        cancelUpload();
        window.location.reload();
      } else {
        toast(xhr.responseText || 'Upload failed', 'error');
        btn.disabled = false;
        btn.textContent = 'Upload track';
      }
    };
    xhr.onerror = () => { toast('Upload failed', 'error'); btn.disabled = false; btn.textContent = 'Upload track'; };
    xhr.send(formData);
  } catch(e) {
    toast('Upload failed: ' + e.message, 'error');
    btn.disabled = false;
    btn.textContent = 'Upload track';
  }
}

// ── Track deletion ────────────────────────────────────────────────────────────
async function deleteTrack(trackId) {
  if (!confirm('Delete this track? This cannot be undone.')) return;
  try {
    const fd = new FormData();
    fd.append('track_id', trackId);
    const res = await fetch('/api/track/delete', { method: 'POST', body: fd });
    if (res.ok) {
      const row = document.getElementById('studio-track-' + trackId);
      if (row) row.remove();
      toast('Track deleted');
    } else {
      toast('Failed to delete track', 'error');
    }
  } catch(e) { toast('Failed to delete track', 'error'); }
}

// ── Audio player ──────────────────────────────────────────────────────────────
const audio = document.getElementById('player-audio');
let currentTrackID = null;

function playTrack(id, url, title, artist, cover) {
  // Track previous card state
  if (currentTrackID) {
    const prev = document.querySelector(`[data-track-id="${currentTrackID}"]`);
    if (prev) {
      prev.querySelector('.play-icon')?.classList.remove('hidden');
      prev.querySelector('.pause-icon')?.classList.add('hidden');
    }
  }

  currentTrackID = id;
  audio.src = url;
  audio.load();

  document.getElementById('player-title').textContent = title;
  document.getElementById('player-artist').textContent = artist;

  const coverEl = document.getElementById('player-cover-img');
  if (cover) {
    coverEl.style.backgroundImage = `url(${cover})`;
    coverEl.style.backgroundSize = 'cover';
    coverEl.style.backgroundPosition = 'center';
  } else {
    coverEl.style.backgroundImage = '';
  }

  document.getElementById('player-bar').classList.remove('hidden');
  audio.play().then(() => {
    updatePlayPauseUI(true);
    // Send play count
    if (id && /^[0-9a-f-]{36}$/.test(id)) {
      fetch('/api/track/play', {method:'POST', headers:{'Content-Type':'application/x-www-form-urlencoded'}, body:'track_id='+id});
    }
    // Update card
    const card = document.querySelector(`[data-track-id="${id}"]`);
    if (card) {
      card.querySelector('.play-icon')?.classList.add('hidden');
      card.querySelector('.pause-icon')?.classList.remove('hidden');
    }
  }).catch(()=>{});
}

function togglePlayerPlay() {
  if (audio.paused) {
    audio.play();
    updatePlayPauseUI(true);
  } else {
    audio.pause();
    updatePlayPauseUI(false);
  }
}
function updatePlayPauseUI(playing) {
  document.getElementById('player-play-icon').classList.toggle('hidden', playing);
  document.getElementById('player-pause-icon').classList.toggle('hidden', !playing);
}
function closePlayer() {
  audio.pause();
  audio.src = '';
  document.getElementById('player-bar').classList.add('hidden');
  if (currentTrackID) {
    const card = document.querySelector(`[data-track-id="${currentTrackID}"]`);
    if (card) {
      card.querySelector('.play-icon')?.classList.remove('hidden');
      card.querySelector('.pause-icon')?.classList.add('hidden');
    }
  }
  currentTrackID = null;
}
function seekRelative(secs) {
  audio.currentTime = Math.max(0, Math.min(audio.duration || 0, audio.currentTime + secs));
}
function seekToClick(event) {
  const rect = event.currentTarget.getBoundingClientRect();
  const pct = (event.clientX - rect.left) / rect.width;
  if (audio.duration) audio.currentTime = pct * audio.duration;
}
function setVolume(val) {
  audio.volume = val / 100;
}
function fmtTime(s) {
  if (!isFinite(s)) return '0:00';
  const m = Math.floor(s / 60);
  const sec = Math.floor(s % 60);
  return m + ':' + String(sec).padStart(2, '0');
}
audio.addEventListener('timeupdate', () => {
  const pct = audio.duration ? (audio.currentTime / audio.duration) * 100 : 0;
  document.getElementById('player-progress-fill').style.width = pct + '%';
  document.getElementById('player-current').textContent = fmtTime(audio.currentTime);
  document.getElementById('player-total').textContent = fmtTime(audio.duration);
});
audio.addEventListener('ended', () => {
  updatePlayPauseUI(false);
  if (currentTrackID) {
    const card = document.querySelector(`[data-track-id="${currentTrackID}"]`);
    if (card) {
      card.querySelector('.play-icon')?.classList.remove('hidden');
      card.querySelector('.pause-icon')?.classList.add('hidden');
    }
  }
});
audio.volume = 0.8;
