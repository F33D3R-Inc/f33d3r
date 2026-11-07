// frequency-media.js — carries the audio of the Frequency a person is in. It holds
// a connection and nothing else. Whether to be connected comes from the dock the
// server renders in the Shell, and whether the microphone is live comes from the
// permission Auralis grants. A press of Mute goes to the server like any other
// control; the one thing done here at once is to stop the microphone, so a person's
// voice never depends on a round trip.

(function () {
  'use strict';

  if (window.__F33D3R_FREQUENCY_MEDIA__) return;
  window.__F33D3R_FREQUENCY_MEDIA__ = true;

  var CLIENT_SRC = '/static/js/vendor/livekit-client-2.23.0.umd.js';
  var RETRY_MAX_MS = 30000;

  var dockMount = document.getElementById('frequency-dock');
  var audioMount = document.getElementById('frequency-media');
  var unlock = document.getElementById('frequency-audio-unlock');
  if (!dockMount || !audioMount || !unlock) return;

  var clientLoading = null;
  var current = null; // { id, room, attempts, timer }
  var speaking = [];

  function loadClient() {
    if (window.LivekitClient) return Promise.resolve(window.LivekitClient);
    if (!clientLoading) {
      clientLoading = new Promise(function (resolve, reject) {
        var script = document.createElement('script');
        script.src = CLIENT_SRC;
        script.onload = function () { resolve(window.LivekitClient); };
        script.onerror = function () { clientLoading = null; reject(new Error('audio client failed to load')); };
        document.head.appendChild(script);
      });
    }
    return clientLoading;
  }

  function wantedRoom() {
    var dock = dockMount.querySelector('[data-frequency-id][data-viewer-role]');
    if (!dock || dock.dataset.state !== 'live' || dock.dataset.viewerRole === 'none') return null;
    return dock.dataset.frequencyId;
  }

  function markSpeakers() {
    document.querySelectorAll('[data-media-identity]').forEach(function (el) {
      el.classList.toggle('is-speaking', speaking.indexOf(el.dataset.mediaIdentity) !== -1);
    });
  }

  function clearAudio() {
    audioMount.replaceChildren();
    unlock.hidden = true;
    speaking = [];
    markSpeakers();
  }

  function stopMic(session) {
    if (!session.room) return;
    var LK = window.LivekitClient;
    var local = session.room.localParticipant;
    var mic = local.getTrackPublication(LK.Track.Source.Microphone);
    if (mic && mic.track) local.unpublishTrack(mic.track, true);
  }

  // The microphone could not open, so the person is not speaking whatever the grant
  // says. The server hears that, shows the muted state and tells them why.
  function micFailed(session) {
    window.htmx.ajax('POST', '/events', {
      values: { event_type: 'frequency.mic_denied', frequency_id: session.id },
      swap: 'none',
    });
  }

  function followGrant(session) {
    var local = session.room.localParticipant;
    if (local.permissions && local.permissions.canPublish) {
      local.setMicrophoneEnabled(true).catch(function () { micFailed(session); });
    } else {
      stopMic(session);
    }
  }

  function retry(session) {
    if (current !== session) return;
    var delay = Math.min(RETRY_MAX_MS, 1000 * Math.pow(2, session.attempts));
    session.attempts += 1;
    session.timer = setTimeout(function () { if (current === session) connect(session); }, delay);
  }

  function watch(session, LK) {
    var room = session.room;
    room.on(LK.RoomEvent.TrackSubscribed, function (track) {
      if (track.kind === LK.Track.Kind.Audio) audioMount.appendChild(track.attach());
    });
    room.on(LK.RoomEvent.TrackUnsubscribed, function (track) {
      track.detach().forEach(function (el) { el.remove(); });
    });
    room.on(LK.RoomEvent.AudioPlaybackStatusChanged, function () {
      unlock.hidden = room.canPlaybackAudio;
    });
    room.on(LK.RoomEvent.ActiveSpeakersChanged, function (people) {
      speaking = people.map(function (p) { return p.identity; });
      markSpeakers();
    });
    room.localParticipant.on(LK.ParticipantEvent.ParticipantPermissionsChanged, function () {
      followGrant(session);
    });
    room.on(LK.RoomEvent.Disconnected, function (reason) {
      if (current !== session) return;
      clearAudio();
      // Removed, ended, or playing in another tab: the server's dock says what is
      // next. Anything else is the network, which comes back.
      var final = reason === LK.DisconnectReason.PARTICIPANT_REMOVED ||
        reason === LK.DisconnectReason.ROOM_DELETED ||
        reason === LK.DisconnectReason.DUPLICATE_IDENTITY;
      if (!final) retry(session);
    });
  }

  function connect(session) {
    loadClient().then(function (LK) {
      return fetch('/frequencies/' + session.id + '/media-token', {
        method: 'POST',
        credentials: 'same-origin',
      }).then(function (res) {
        if (current !== session) return null;
        if (res.status >= 400 && res.status < 500) return null; // refused, not failed
        if (!res.ok) throw new Error('audio token: ' + res.status);
        return res.json();
      }).then(function (grant) {
        if (!grant || current !== session) return null;
        if (session.room) session.room.removeAllListeners();
        session.room = new LK.Room({
          adaptiveStream: false,
          dynacast: false,
          audioCaptureDefaults: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
        });
        watch(session, LK);
        var url = location.origin.replace(/^http/, 'ws');
        return session.room.connect(url, grant.token).then(function () {
          if (current !== session) { session.room.disconnect(); return; }
          session.attempts = 0;
          followGrant(session);
          session.room.startAudio().catch(function () {}).then(function () {
            unlock.hidden = session.room.canPlaybackAudio;
          });
        });
      });
    }).catch(function () {
      retry(session);
    });
  }

  function sync() {
    var wanted = wantedRoom();
    if ((current && current.id) === wanted || (!current && !wanted)) return;
    if (current) {
      var leaving = current;
      current = null;
      clearTimeout(leaving.timer);
      if (leaving.room) leaving.room.disconnect();
      clearAudio();
    }
    if (wanted) {
      current = { id: wanted, room: null, attempts: 0, timer: null };
      connect(current);
    }
  }

  unlock.addEventListener('click', function () {
    if (current && current.room) current.room.startAudio().then(function () { unlock.hidden = true; });
  });

  document.addEventListener('submit', function (e) {
    if (current && e.target.matches && e.target.matches('[data-media-mute]')) stopMic(current);
  }, true);

  // The form carries a time the browser picked in the person's zone. The server is
  // given the instant, so it never has to guess a zone.
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!form.querySelectorAll) return;
    form.querySelectorAll('input[data-utc-into]').forEach(function (input) {
      var into = form.querySelector('input[name="' + input.dataset.utcInto + '"]');
      if (into) into.value = input.value ? new Date(input.value).toISOString() : '';
    });
  }, true);

  // A clip starts where the person has the replay.
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!form.matches || !form.matches('[data-clip-form]')) return;
    var player = document.querySelector('.freq-replay__audio');
    form.querySelector('input[name="start_seconds"]').value = player ? String(Math.floor(player.currentTime)) : '0';
  }, true);

  document.addEventListener('htmx:afterRequest', function (e) {
    var form = e.target;
    if (e.detail.successful && form.matches && form.matches('[data-reset-on-success]')) form.reset();
  });

  // A finished clip opens in the compose box, and is posted under the person's own key.
  document.addEventListener('click', function (e) {
    var button = e.target.closest && e.target.closest('[data-post-video]');
    if (button && window.attachVideoToCompose) window.attachVideoToCompose(JSON.parse(button.dataset.postVideo));
  });

  document.addEventListener('click', function (e) {
    var button = e.target.closest && e.target.closest('[data-copy-url]');
    if (!button || !navigator.clipboard) return;
    navigator.clipboard.writeText(location.origin + button.dataset.copyUrl).then(function () {
      button.textContent = 'Link copied';
      setTimeout(function () { button.textContent = 'Copy link'; }, 2000);
    });
  });

  // Display only: times arrive in UTC and read in the person's own zone, and a
  // Fragment that was just replaced gets its speaking ring back.
  var localFormat = new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
  function dress() {
    document.querySelectorAll('time[data-local]:not([data-dressed])').forEach(function (el) {
      el.dataset.dressed = '1';
      el.textContent = localFormat.format(new Date(el.dateTime));
    });
    if (speaking.length) markSpeakers();
  }
  var dressQueued = false;
  new MutationObserver(function () {
    if (dressQueued) return;
    dressQueued = true;
    requestAnimationFrame(function () { dressQueued = false; dress(); });
  }).observe(document.body, { childList: true, subtree: true });
  dress();

  new MutationObserver(sync).observe(dockMount, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ['data-state', 'data-viewer-role'],
  });
  sync();
})();
