// music.js — listen/artist mode, genre filter and the audio player.
// Registered as a Shell page: executed once per session, the mount runs on
// every arrival of the music Playground; its teardown stops playback because
// the player element leaves with the page.
F33D3R.page('music', function () {
  // Server values from <meta> tags — no template injection in JS
  const SESSION_ID    = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
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

  // Restore mode from localStorage on arrival
  (function () {
    const saved = localStorage.getItem('f33d3r-music-mode');
    if (saved === 'artist') setMusicMode('artist');
  })();

  // ── Genre filter ──────────────────────────────────────────────────────────────
  // The track list is a server Facet; the URL is pushed through htmx so
  // back/forward restores the page the server draws for that genre.
  function filterGenre(btn, genre) {
    document.querySelectorAll('.genre-chip').forEach(c => c.classList.remove('active'));
    btn.classList.add('active');
    const url = genre ? '/music/tracks?genre=' + encodeURIComponent(genre) : '/music/tracks';
    const pageUrl = genre ? '/music?genre=' + encodeURIComponent(genre) : '/music';
    htmx.ajax('GET', url, { target: '#track-feed', swap: 'innerHTML', push: pageUrl });
  }

  // ── Track deletion ────────────────────────────────────────────────────────────
  // The server answers with the card's tombstone Fragment and pushes the same
  // Fragment to everyone watching the track; this page only applies it.
  async function deleteTrack(trackId) {
    if (!confirm('Delete this track? This cannot be undone.')) return;
    const fd = new FormData();
    fd.append('track_id', trackId);
    const res = await fetch('/api/track/delete', { method: 'POST', body: fd });
    if (res.ok) {
      document.getElementById('crt-track-' + trackId)?.remove();
      document.getElementById('music-track-' + trackId)?.remove();
      toast('Track deleted');
    } else {
      toast('Failed to delete track', 'error');
    }
  }

  // ── Audio player ──────────────────────────────────────────────────────────────
  // Playback only. The page reports what happened — the track started, ended,
  // or was left at a position — and the server decides what that means (a
  // play, a completion, a skip), counts it and pushes the counts back as
  // Fragments. No listening state is kept here.
  const audio = document.getElementById('player-audio');
  let currentTrackID = null;

  function report(event, id) {
    if (!id || !audio) return;
    const body = new URLSearchParams({
      track_id: id,
      event: event,
      position_secs: String(isFinite(audio.currentTime) ? audio.currentTime : 0),
      duration_secs: String(isFinite(audio.duration) ? audio.duration : 0),
      session_id: window._sessionID || SESSION_ID
    });
    fetch('/api/track/play', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: body.toString(),
      keepalive: true
    }).catch(() => {});
  }

  function setCardPlaying(id, playing) {
    const card = id && document.querySelector(`[data-track-id="${id}"]`);
    if (!card) return;
    card.querySelector('.play-icon')?.classList.toggle('hidden', playing);
    card.querySelector('.pause-icon')?.classList.toggle('hidden', !playing);
  }

  function playTrack(id, url, title, artist, cover) {
    if (!audio) return;
    if (currentTrackID && currentTrackID !== id) {
      report('left', currentTrackID);
      setCardPlaying(currentTrackID, false);
    }
    currentTrackID = id;
    audio.src = url;
    audio.load();
    document.getElementById('player-title').textContent  = title || '--';
    document.getElementById('player-artist').textContent = artist || '--';
    const coverEl = document.getElementById('player-cover-img');
    if (coverEl) {
      coverEl.style.backgroundImage    = cover ? `url(${cover})` : '';
      coverEl.style.backgroundSize     = 'cover';
      coverEl.style.backgroundPosition = 'center';
    }
    document.getElementById('player-bar')?.classList.remove('hidden');
    audio.play().then(() => {
      updatePlayPauseUI(true);
      setCardPlaying(id, true);
      report('started', id);
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
    if (currentTrackID && !audio.ended) report('left', currentTrackID);
    audio.pause();
    audio.src = '';
    document.getElementById('player-bar')?.classList.add('hidden');
    setCardPlaying(currentTrackID, false);
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
      report('ended', currentTrackID);
      setCardPlaying(currentTrackID, false);
    });
  }

  window.setMusicMode     = setMusicMode;
  window.filterGenre      = filterGenre;
  window.deleteTrack      = deleteTrack;
  window.playTrack        = playTrack;
  window.togglePlayerPlay = togglePlayerPlay;
  window.closePlayer      = closePlayer;
  window.seekRelative     = seekRelative;
  window.seekToClick      = seekToClick;
  window.setVolume        = setVolume;

  return function teardown() {
    // Leaving the page mid-track is reported as left; keepalive carries it out.
    if (audio && currentTrackID && !audio.ended && audio.currentTime > 0) report('left', currentTrackID);
    if (audio) { try { audio.pause(); audio.removeAttribute('src'); audio.load(); } catch (e) {} }
  };
});
