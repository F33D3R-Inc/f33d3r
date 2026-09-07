// live-broadcast.js — broadcaster surface · camera wiring and copy affordances.
//
// Reuses the existing Capture Substrate (CaptureKit.Controller + PreviewSurface)
// rather than opening a second camera stack. This file holds the local preview
// and hands the MediaStream to the ingest transport; stream status, viewer
// tallies and the end-of-broadcast decision are all server state and arrive as
// rendered Fragments.
//
// Which of the two publish paths this broadcast uses is the server's decision,
// carried on the stage as data-source. When it is not "browser" this file opens
// no camera at all — a desktop encoder is already publishing into the stream and
// a second publisher would be refused by the media server.
//
// The Shell executes this file once per session. The mount registered at the
// bottom binds the stage of whichever Playground arrives and returns the
// teardown that releases the camera when that page leaves.

(function () {
  'use strict';

  if (window.__F33D3R_LIVE_BCAST__) return;
  window.__F33D3R_LIVE_BCAST__ = true;

  var ctl = null;
  var preview = null;
  var stage = null;

  function say(msg) {
    var el = stage && stage.querySelector('[data-live-bcast-status]');
    if (el) el.textContent = msg || '';
  }

  // Copy feedback belongs beside the field that was copied when there is one,
  // and on the stage's own status line otherwise.
  function sayCopy(el, msg) {
    var facet = el.closest ? el.closest('[data-facet-id]') : null;
    var target = facet && facet.querySelector('[data-live-copy-status]');
    if (target) { target.textContent = msg; return; }
    say(msg);
  }

  // Clipboard access needs a secure context. A phone reaching this page over
  // plain http on a LAN address has neither that nor a camera, so the selected
  // text path is the fallback rather than a silent failure.
  function copyText(text, field) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      if (!field || !field.select) {
        reject(new Error('this browser cannot copy without a secure context'));
        return;
      }
      field.removeAttribute('readonly');
      field.select();
      field.setSelectionRange(0, text.length);
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (err) { ok = false; }
      field.setAttribute('readonly', 'readonly');
      if (ok) resolve(); else reject(new Error('the browser refused the copy'));
    });
  }

  function mirror(facing) {
    var v = stage && stage.querySelector('[data-live-preview]');
    if (!v) return;
    v.classList.toggle('camera-preview--front', facing === 'user');
    v.classList.toggle('camera-preview--rear', facing !== 'user');
    stage.setAttribute('data-facing', facing);
  }

  // The media pipeline registers window.F33D3R_LiveIngest = { start(streamId,
  // stream), stop() }. Until it does, the broadcaster is told plainly that the
  // camera is live locally but nothing is being transmitted — never a silent
  // preview that looks like a broadcast.
  function handOff(streamID, mediaStream) {
    var ingest = window.F33D3R_LiveIngest;
    if (!ingest || typeof ingest.start !== 'function') {
      say('Camera ready. Transmission has not started: no ingest transport is available in this browser session.');
      return;
    }
    try {
      ingest.start(streamID, mediaStream);
      say('');
    } catch (err) {
      console.error('[live] ingest start failed', err);
      say('Camera ready, but the broadcast could not be transmitted: ' + (err && err.message ? err.message : 'ingest error'));
    }
  }

  function open(facing) {
    var K = window.CaptureKit;
    if (!K || !K.Controller) {
      say('The camera substrate did not load. Reload the page to broadcast.');
      return;
    }
    if (!ctl) {
      ctl = new K.Controller(new K.StateMachine(), new K.SourceRegistry());
    }
    // The preview always binds to the stage on screen now — a stage from an
    // earlier arrival of this page is gone.
    preview = new K.PreviewSurface(stage.querySelector('[data-live-preview]'));
    ctl.open({ mode: 'video', facing: facing })
      .then(function (mediaStream) {
        if (!mediaStream) return;
        if (!stage || !document.contains(stage)) { stop(); return; }
        preview.bind(mediaStream, facing);
        mirror(facing);
        handOff(stage.getAttribute('data-stream-id'), mediaStream);
      })
      .catch(function (err) {
        console.error('[live] camera open failed', err);
        var denied = err && (err.name === 'NotAllowedError' || err.name === 'SecurityError');
        say(denied
          ? 'Camera and microphone access was denied. Allow them in your browser settings to broadcast.'
          : 'The camera could not be opened: ' + ((err && err.name) || 'unknown error'));
      });
  }

  function stop() {
    var ingest = window.F33D3R_LiveIngest;
    if (ingest && typeof ingest.stop === 'function') {
      try { ingest.stop(); } catch (err) { console.error('[live] ingest stop failed', err); }
    }
    if (ctl) ctl.stop();
    if (preview) preview.clear();
  }

  function onClick(e) {
    var el = e.target.closest ? e.target.closest('[data-live-action]') : null;
    if (!el || !stage || !stage.contains(el)) return;
    var action = el.getAttribute('data-live-action');

    if (action === 'bcast-flip') {
      if (!ctl) return;
      var next = (stage.getAttribute('data-facing') === 'user') ? 'environment' : 'user';
      ctl.open({ mode: 'video', facing: next })
        .then(function (ms) {
          if (!ms) return;
          preview.bind(ms, next);
          mirror(next);
          handOff(stage.getAttribute('data-stream-id'), ms);
        })
        .catch(function (err) { say('Could not switch camera: ' + ((err && err.name) || 'error')); });
      return;
    }

    if (action === 'bcast-mic') {
      if (!ctl || !ctl.stream) return;
      var on = el.getAttribute('aria-pressed') === 'true';
      ctl.setMic(on); // aria-pressed true means "muted", so pressing restores audio
      el.setAttribute('aria-pressed', on ? 'false' : 'true');
      el.setAttribute('aria-label', on ? 'Mute microphone' : 'Unmute microphone');
      return;
    }

    if (action === 'copy-link') {
      var path = el.getAttribute('data-live-url') || '';
      if (!path) { say('There is no link to copy.'); return; }
      var full = new URL(path, window.location.href).href;
      copyText(full, null)
        .then(function () { sayCopy(el, 'Stream link copied.'); })
        .catch(function (err) {
          console.error('[live] copy link failed', err);
          sayCopy(el, 'Could not copy the link: ' + ((err && err.message) || 'clipboard refused') + '. The link is ' + full);
        });
      return;
    }

    if (action === 'copy-field') {
      var field = document.getElementById(el.getAttribute('data-live-copy-target') || '');
      if (!field) { sayCopy(el, 'That field is no longer on the page.'); return; }
      copyText(field.value, field)
        .then(function () { sayCopy(el, 'Copied. Paste it into your encoder.'); })
        .catch(function (err) {
          console.error('[live] copy field failed', err);
          sayCopy(el, 'Could not copy: ' + ((err && err.message) || 'clipboard refused') + '. Select the field and copy it by hand.');
        });
      return;
    }
  }

  // Document-level plumbing, installed by this file's single execution.
  document.addEventListener('click', onClick);
  window.addEventListener('pagehide', stop);

  // The mount: bind the stage the server rendered into this Playground.
  window.F33D3R.page('live-broadcast', function (root) {
    var scope = root && root.querySelector ? root : document;
    stage = scope.querySelector('.live-bcast');
    if (!stage) return;

    // Whether this device captures is the server's decision, and the stage the
    // server last rendered is where that decision is written. This re-reads it
    // whenever the stage element is replaced, and does nothing else.
    //
    // The stage is replaced two ways and only one of them is htmx: the owner's
    // own "End broadcast" arrives as an htmx swap, but every other way a
    // broadcast ends — the publisher dropping, the media server restarting, the
    // ladder dying, a moderation block — is decided on the server with no
    // request to answer, and its Fragment is applied by the FA Live runtime,
    // which replaces the element directly and fires no htmx event. Watching for
    // one mechanism left the camera running under a broadcast that had ended.
    //
    // A replacement is not by itself an ending, so the node is re-read rather
    // than assumed gone: a stage that comes back still asking this device to
    // publish keeps publishing, and its fresh <video> is re-bound to the camera
    // that is already open. Only a stage that has gone, or one the server no
    // longer marks as this device's to publish from, releases the hardware.
    var streamID = stage.getAttribute('data-stream-id');
    var watcher = new MutationObserver(function () {
      if (document.contains(stage)) {
        // The server's LIVE badge arriving supersedes the transport's own
        // progress line: "connected, waiting to be packaged" is no longer the
        // news once the platform says the stream is on air. A fault line is
        // left alone — it stands until the broadcaster acts on it.
        var status = stage.querySelector('[data-live-bcast-status]');
        if (status && status.getAttribute('data-live-bcast-status-kind') === 'transport' &&
            stage.querySelector('.live-badge--live')) {
          status.textContent = '';
          status.removeAttribute('data-live-bcast-status-kind');
        }
        return;
      }

      var next = streamID
        ? document.querySelector('.live-bcast[data-stream-id="' + streamID + '"]')
        : null;
      if (!next || next.getAttribute('data-source') !== 'browser') {
        watcher.disconnect();
        stop();
        return;
      }

      stage = next;
      var facing = stage.getAttribute('data-facing') || 'environment';
      if (ctl && ctl.stream) {
        preview = new window.CaptureKit.PreviewSurface(stage.querySelector('[data-live-preview]'));
        preview.bind(ctl.stream, facing);
      }
    });
    watcher.observe(document.documentElement, { childList: true, subtree: true });

    // The page leaves: the camera and the ingest go with it.
    var teardown = function () {
      watcher.disconnect();
      stop();
      stage = null;
    };

    if (stage.getAttribute('data-source') !== 'browser') return teardown;

    // getUserMedia is refused outside a secure context. Saying so beats a bare
    // NotAllowedError the broadcaster cannot act on.
    if (!window.isSecureContext) {
      say('This page was not loaded over https (or localhost), so the browser will not give it a camera. ' +
          'Open f33d3r over https to broadcast from this device, or use the OBS path.');
      return teardown;
    }
    open(stage.getAttribute('data-facing') || 'environment');
    return teardown;
  });
})();
