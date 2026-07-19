// Server values from <meta> tags — no template injection in JS
const SESSION_ID    = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
const MY_PIAL       = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE     = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';

// ── Mode toggle ───────────────────────────────────────────────────────────────
function setMusicMode(mode) {
  const listenPanel  = document.getElementById('music-listen');
  const artistPanel  = document.getElementById('music-artist');
  const listenBtn    = document.getElementById('mode-listen-btn');
  const artistBtn    = document.getElementById('mode-artist-btn');
  if (!listenPanel || !artistPanel) return;

  listenPanel.classList.toggle('hidden', mode !== 'listen');
  artistPanel.classList.toggle('hidden', mode !== 'artist');
  listenBtn.classList.toggle('active', mode === 'listen');
  artistBtn.classList.toggle('active', mode === 'artist');
  listenBtn.setAttribute('aria-selected', mode === 'listen');
  artistBtn.setAttribute('aria-selected', mode === 'artist');
  localStorage.setItem('f33d3r-music-mode', mode);
}

// Restore mode from localStorage on page load
(function () {
  const saved = localStorage.getItem('f33d3r-music-mode');
  if (saved === 'artist') setMusicMode('artist');
})();

// ── Genre filter ──────────────────────────────────────────────────────────────
function filterGenre(btn, genre) {
  document.querySelectorAll('.genre-chip').forEach(c => c.classList.remove('active'));
  btn.classList.add('active');
  const url = genre ? '/music/tracks?genre=' + encodeURIComponent(genre) : '/music/tracks';
  htmx.ajax('GET', url, { target: '#track-feed', swap: 'innerHTML' });
  history.pushState({genre: genre}, '', genre ? '/music?genre=' + encodeURIComponent(genre) : '/music');
}

// ── Track deletion ────────────────────────────────────────────────────────────
async function deleteTrack(trackId) {
  if (!confirm('Delete this track? This cannot be undone.')) return;
  try {
    const fd = new FormData();
    fd.append('track_id', trackId);
    const res = await fetch('/api/track/delete', { method: 'POST', body: fd });
    if (res.ok) {
      document.getElementById('crt-track-' + trackId)?.remove();
      toast('Track deleted');
    } else {
      toast('Failed to delete track', 'error');
    }
  } catch (e) {
    toast('Failed to delete track', 'error');
  }
}

// ── Audio player ──────────────────────────────────────────────────────────────
const audio = document.getElementById('player-audio');
let currentTrackID = null;

function playTrack(id, url, title, artist, cover) {
  if (currentTrackID && currentTrackID !== id) {
    const prev = document.querySelector(`[data-track-id="${currentTrackID}"]`);
    if (prev) {
      prev.querySelector('.play-icon')?.classList.remove('hidden');
      prev.querySelector('.pause-icon')?.classList.add('hidden');
    }
  }
  currentTrackID = id;
  audio.src = url;
  audio.load();
  document.getElementById('player-title').textContent  = title || '--';
  document.getElementById('player-artist').textContent = artist || '--';

  const coverEl = document.getElementById('player-cover-img');
  if (coverEl) {
    coverEl.style.backgroundImage   = cover ? `url(${CSS.escape ? cover : cover})` : '';
    coverEl.style.backgroundSize    = 'cover';
    coverEl.style.backgroundPosition = 'center';
  }
  document.getElementById('player-bar')?.classList.remove('hidden');
  audio.play().then(() => {
    updatePlayPauseUI(true);
    if (id && /^[0-9a-f-]{36}$/.test(id)) {
      fetch('/api/track/play', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: 'track_id=' + encodeURIComponent(id)
      });
    }
    const card = document.querySelector(`[data-track-id="${id}"]`);
    if (card) {
      card.querySelector('.play-icon')?.classList.add('hidden');
      card.querySelector('.pause-icon')?.classList.remove('hidden');
    }
  }).catch(() => {});
}

function togglePlayerPlay() {
  if (!audio) return;
  if (audio.paused) { audio.play(); updatePlayPauseUI(true); }
  else              { audio.pause(); updatePlayPauseUI(false); }
}
function updatePlayPauseUI(playing) {
  document.getElementById('player-play-icon')?.classList.toggle('hidden', playing);
  document.getElementById('player-pause-icon')?.classList.toggle('hidden', !playing);
}
function closePlayer() {
  if (!audio) return;
  audio.pause();
  audio.src = '';
  document.getElementById('player-bar')?.classList.add('hidden');
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
  if (!audio) return;
  audio.currentTime = Math.max(0, Math.min(audio.duration || 0, audio.currentTime + secs));
}
function seekToClick(event) {
  if (!audio || !audio.duration) return;
  const rect = event.currentTarget.getBoundingClientRect();
  audio.currentTime = ((event.clientX - rect.left) / rect.width) * audio.duration;
}
function setVolume(val) {
  if (audio) audio.volume = val / 100;
}
function fmtTime(s) {
  if (!isFinite(s)) return '0:00';
  const m = Math.floor(s / 60);
  const sec = Math.floor(s % 60);
  return m + ':' + String(sec).padStart(2, '0');
}

if (audio) {
  audio.volume = 0.8;
  audio.addEventListener('timeupdate', () => {
    const pct = audio.duration ? (audio.currentTime / audio.duration) * 100 : 0;
    const fill = document.getElementById('player-progress-fill');
    if (fill) fill.style.width = pct + '%';
    const cur = document.getElementById('player-current');
    const tot = document.getElementById('player-total');
    if (cur) cur.textContent = fmtTime(audio.currentTime);
    if (tot) tot.textContent = fmtTime(audio.duration);
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
}
