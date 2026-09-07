// live-player.js — FA Live watch surface and Playground tiles · media decode only.
//
// This file drives HLS decoders and the player chrome's visual state. It owns
// nothing else: stream status, viewer tallies and chat are server state and
// reach the page as rendered Fragments. The only values written here are
// decoder-level facts — paused, muted, fullscreen, buffering, selected
// rendition, and whether a decoder is currently attached — expressed as data-*
// attributes on the element the server already rendered, so CSS shows the
// matching glyph. Nothing here remembers what is live; it is told.
//
// Three responsibilities:
//
//   1. Play on sight. A live broadcast is not a thing you ask to start. The
//      stage decodes muted, which is the only shape an autoplay policy will
//      honour, and if a browser refuses anyway the refusal is surfaced — the
//      large play control comes back — never swallowed.
//
//   2. Decode what is on screen and nothing else. A Playground can hold many
//      live tiles. Attaching a decoder per tile and running them all at once
//      melts the machine, so a tile's decoder is attached when the tile enters
//      the viewport and destroyed when it leaves. This is resource management,
//      not application state: the tile still learns it went dark from a server
//      Fragment, exactly as it did before.
//
//   3. Never orphan a decoder. A player Fragment replaced by the server — a
//      broadcast that ended arriving as live_ended over the heartbeat or FA
//      Live — takes its decoder with it. Removal from the document is the
//      signal, so every swap mechanism is covered by one rule.

(function () {
  'use strict';

  if (window.__F33D3R_LIVE_PLAYER__) return;
  window.__F33D3R_LIVE_PLAYER__ = true;

  var HLS_LOCAL = '/static/js/hls.min.js';
  var STAGE_SRC = 'data-f33d-live-hls';       // watch surface / profile stage
  var TILE_SRC  = 'data-f33d-live-card-hls';  // Playground tile
  var DECODERS  = 'video[' + STAGE_SRC + '],video[' + TILE_SRC + ']';

  var hlsLoading = null;
  var attached = new WeakMap(); // video -> { hls, host, kind, retries, timer }
  var wired    = new WeakSet(); // videos whose element listeners are bound
  var observed = new WeakSet(); // tiles handed to the viewport observer
  var onScreen = new Set();     // tiles the observer says are visible right now

  function ensureHlsLib() {
    if (window.Hls) return Promise.resolve(window.Hls);
    if (hlsLoading) return hlsLoading;
    hlsLoading = new Promise(function (resolve, reject) {
      var s = document.createElement('script');
      s.src = HLS_LOCAL;
      s.async = true;
      s.onload = function () { resolve(window.Hls); };
      s.onerror = function () { reject(new Error('hls.js failed to load')); };
      document.head.appendChild(s);
    });
    return hlsLoading;
  }

  // Media Source Extensions, probed against the exact codec shape the ABR ladder
  // publishes. This is the question that decides the engine, and it is asked
  // before canPlayType because canPlayType is not an answer: Chrome replies
  // "maybe" to the HLS mime type on every platform, which sent every Chrome
  // viewer down the built-in path — no hls.js, no rendition control, no retry
  // policy of ours, and a media playlist re-fetched tens of times a second.
  function mseSupported() {
    var MS = window.ManagedMediaSource || window.MediaSource;
    if (!MS || typeof MS.isTypeSupported !== 'function') return false;
    try { return MS.isTypeSupported('video/mp4;codecs="avc1.42E01E,mp4a.40.2"'); }
    catch (_) { return false; }
  }

  // The fallback, and only the fallback: an engine with no MSE that decodes HLS
  // itself. That is iPhone Safari, where it is the correct and only answer.
  function nativeHls(video) {
    return video.canPlayType('application/vnd.apple.mpegurl') !== '';
  }

  function srcOf(video) {
    return video.getAttribute(STAGE_SRC) || video.getAttribute(TILE_SRC) || '';
  }

  function isTile(video) { return video.hasAttribute(TILE_SRC); }

  // The host is the element the server rendered with data-decode on it: the
  // player root for a stage, the tile anchor for a card. CSS keys off it.
  function hostOf(video) {
    return video.closest ? video.closest('[data-decode]') : null;
  }

  function rootOf(el) { return el.closest ? el.closest('.live-player') : null; }

  function setDecode(host, state) {
    if (host) host.setAttribute('data-decode', state);
  }

  function setStatus(root, msg) {
    var el = root && root.querySelector('[data-live-error]');
    if (el) { el.textContent = msg || ''; el.hidden = !msg; }
  }

  // ── quality menu ───────────────────────────────────────────────────────────

  // Mark the rungs the master manifest actually publishes. A rung the ladder
  // advertises but the manifest omits is disabled rather than silently ignored.
  function syncLadder(root, levels) {
    var menu = root && root.querySelector('.live-quality__menu');
    if (!menu) return;
    var items = menu.querySelectorAll('[data-height]');
    for (var i = 0; i < items.length; i++) {
      var h = parseInt(items[i].getAttribute('data-height'), 10);
      var idx = -1;
      for (var j = 0; j < levels.length; j++) {
        if (levels[j].height === h) { idx = j; break; }
      }
      if (idx === -1) {
        items[i].disabled = true;
        items[i].setAttribute('aria-disabled', 'true');
        items[i].title = 'Not published for this stream';
      } else {
        items[i].disabled = false;
        items[i].removeAttribute('aria-disabled');
        items[i].setAttribute('data-level', String(idx));
      }
    }
  }

  function markActiveRung(root, levelIdx) {
    if (!root) return;
    var items = root.querySelectorAll('.live-quality__item');
    for (var i = 0; i < items.length; i++) {
      var lv = items[i].getAttribute('data-level');
      var on = (lv !== null && parseInt(lv, 10) === levelIdx);
      items[i].classList.toggle('is-active', on);
      items[i].setAttribute('aria-checked', on ? 'true' : 'false');
    }
  }

  function openQuality(root, open) {
    var q = root.querySelector('.live-quality');
    if (!q) return;
    var menu = q.querySelector('.live-quality__menu');
    var gear = q.querySelector('.live-quality__gear');
    q.setAttribute('data-open', open ? '1' : '0');
    if (menu) menu.hidden = !open;
    if (gear) gear.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open && menu) {
      var first = menu.querySelector('.live-quality__item:not([disabled])');
      if (first) first.focus();
    } else if (gear) {
      gear.focus();
    }
  }

  // ── playback ───────────────────────────────────────────────────────────────

  // Jump to the live edge. A live viewer who tabs away, or a tile that scrolls
  // back on screen, must resume at "now" and not replay what it missed.
  function seekLive(video, hls) {
    try {
      if (hls && hls.liveSyncPosition) { video.currentTime = hls.liveSyncPosition; return; }
      if (video.seekable && video.seekable.length) {
        video.currentTime = video.seekable.end(video.seekable.length - 1);
      }
    } catch (_) { /* not seekable yet */ }
  }

  // Start decoding, and treat a refusal as a refusal.
  //
  // Autoplay is granted to a muted, inline element and to nothing else, so a
  // rejected promise means either the element was not in that shape or the
  // engine declined outright. The first case is repairable and is repaired
  // once; the second is reported by putting the play control back, so the
  // surface asks for the tap it needs instead of sitting dead.
  //
  // Only the autoplay refusal is repaired by muting, and it is named: the
  // engine reports it as NotAllowedError and nothing else. This used to mute
  // on ANY rejection, and play() is asked for again on every rejoin and
  // retry for the life of the decode — so a viewer who had turned the sound
  // on was silently muted again the first time a retry's play() was
  // interrupted by the seek or reload that accompanied it (AbortError), and
  // told to tap for sound over a broadcast they had already unmuted.
  //
  // An AbortError is a request superseded by a newer load, seek or pause;
  // whichever issued that owns the outcome, so it is neither repaired nor
  // reported here.
  function play(video, opts) {
    var host = hostOf(video);
    var rec = attached.get(video);
    if (!video.isConnected) return;

    setDecode(host, 'starting');
    if (!opts || !opts.keepPosition) seekLive(video, rec && rec.hls);

    var p;
    try { p = video.play(); } catch (err) { blocked(video, host, err); return; }
    if (!p || typeof p.then !== 'function') return; // pre-promise engine

    p.catch(function (err) {
      var name = err && err.name;
      // Repair the one condition an engine will forgive, then ask once more.
      if (name === 'NotAllowedError' && !video.muted) {
        video.muted = true;
        return video.play();
      }
      if (name === 'AbortError') return;
      throw err;
    }).catch(function (err) {
      blocked(video, host, err);
    });
  }

  function blocked(video, host, err) {
    if (!video.isConnected) return;
    setDecode(host, 'blocked');
    var root = rootOf(video);
    if (root) {
      root.setAttribute('data-paused', '1');
      syncLabels(root, video);
      setStatus(root, 'Your browser blocked playback — press play to watch');
    }
    if (window.console) console.warn('[live] autoplay refused', err && err.name, err && err.message);
  }

  // ── stall supervision ──────────────────────────────────────────────────────
  //
  // A live decode can stop advancing without erroring: the playlist stops being
  // written, or is closed out under the decoder, and the element sits at
  // readyState 2 forever with paused === false and no event to say so. Left
  // alone that is a frozen picture the viewer is given no account of — which is
  // exactly what "the stream is stuck" looks like from the outside.
  //
  // So the decode is supervised. Whether the broadcast is over remains the
  // server's word and arrives as a Fragment; this only reports what the decoder
  // is actually doing and keeps trying to rejoin the live edge while it is
  // worth doing.
  var STALL_SPEAK   = 3;   // seconds frozen before the surface says so
  var STALL_REJOIN  = 6;   // seconds between attempts to rejoin the live edge
  var STALL_GIVEUP  = 30;  // seconds frozen before the message becomes final
  var STALL_PATIENT = 30;  // seconds between attempts once it has become final
  var TILE_GIVEUP   = 20;  // a frozen tile is released — the poster is honest

  function watch(video, rec) {
    rec.lastT = -1;
    rec.stall = 0;
    rec.watch = setInterval(function () {
      if (attached.get(video) !== rec || !video.isConnected) { clearInterval(rec.watch); return; }
      var root = rootOf(video);

      if (video.paused || video.seeking) { rec.lastT = video.currentTime; rec.stall = 0; return; }

      if (video.currentTime > rec.lastT + 0.01) {
        rec.lastT = video.currentTime;
        if (rec.stall) {
          rec.stall = 0;
          if (root) { root.setAttribute('data-buffering', '0'); setStatus(root, ''); }
        }
        return;
      }

      rec.stall++;
      if (rec.stall === STALL_SPEAK && root) {
        root.setAttribute('data-buffering', '1');
        setStatus(root, 'Waiting for video from this broadcast');
      }
      if (rec.tile && rec.stall >= TILE_GIVEUP) { detach(video); return; }
      if (rec.stall === STALL_GIVEUP && root) {
        setStatus(root, 'No video is arriving from this broadcast');
      }
      // Polite, then patient. A surface whose broadcast has been silent for
      // half a minute keeps a slow watch rather than a fast one — the server
      // will replace this Fragment the moment it has something to say.
      if (rec.stall % (rec.stall >= STALL_GIVEUP ? STALL_PATIENT : STALL_REJOIN) === 0) rejoin(video, rec);
    }, 1000);
  }

  // Rejoin the live edge. A live viewer is owed "now", not the frame the decode
  // died on.
  function rejoin(video, rec) {
    if (rec.hls) {
      try { rec.hls.stopLoad(); } catch (_) {}
      try { rec.hls.startLoad(-1); } catch (_) {}
    }
    seekLive(video, rec.hls);
    if (video.paused) play(video);
  }

  // ── decoder attach / detach ────────────────────────────────────────────────

  function wire(video) {
    if (wired.has(video)) return;
    wired.add(video);

    video.addEventListener('play', function () {
      var root = rootOf(video);
      if (root) { root.setAttribute('data-paused', '0'); syncLabels(root, video); }
    });
    video.addEventListener('pause', function () {
      var root = rootOf(video);
      if (root) { root.setAttribute('data-paused', '1'); syncLabels(root, video); }
      var host = hostOf(video);
      if (host && host.getAttribute('data-decode') === 'playing') setDecode(host, 'idle');
    });
    video.addEventListener('volumechange', function () {
      var root = rootOf(video);
      if (root) { root.setAttribute('data-muted', video.muted ? '1' : '0'); syncLabels(root, video); }
    });
    video.addEventListener('waiting', function () {
      var root = rootOf(video);
      if (root) root.setAttribute('data-buffering', '1');
    });
    // A live playlist that is closed out ends the element's media. The server
    // decides whether the broadcast is over; this only says the picture stopped.
    video.addEventListener('ended', function () {
      var root = rootOf(video);
      if (root) setStatus(root, 'The video feed stopped');
      setDecode(hostOf(video), 'idle');
      var rec = attached.get(video);
      if (rec && rec.tile) detach(video);
    });
    video.addEventListener('playing', function () {
      var root = rootOf(video);
      if (root) { root.setAttribute('data-buffering', '0'); setStatus(root, ''); }
      setDecode(hostOf(video), 'playing');
      var rec = attached.get(video);
      if (rec) rec.retries = 0;
    });
  }

  function attach(video) {
    if (attached.has(video)) return;
    var src = srcOf(video);
    if (!src) return;
    var host = hostOf(video);

    video.setAttribute('playsinline', '');
    video.setAttribute('webkit-playsinline', '');
    video.muted = true;

    wire(video);

    var rec = { hls: null, host: host, tile: isTile(video), retries: 0, timer: 0, watch: 0, stall: 0, lastT: -1, parsed: false };
    attached.set(video, rec);
    setDecode(host, 'starting');
    watch(video, rec);

    if (!mseSupported()) {
      if (!nativeHls(video)) {
        setStatus(rootOf(video), 'This browser cannot play live video');
        setDecode(host, 'idle');
        return;
      }
      // An engine that decodes HLS itself exposes no rendition control. Say so
      // rather than shipping a gear that does nothing.
      video.src = src;
      video.load();
      var gear = host && host.querySelector('.live-quality__gear');
      if (gear) {
        gear.disabled = true;
        gear.title = 'This browser chooses the rendition automatically';
      }
      play(video);
      return;
    }

    ensureHlsLib().then(function (Hls) {
      if (attached.get(video) !== rec || !video.isConnected) return; // detached while loading
      if (!Hls || !Hls.isSupported()) {
        if (nativeHls(video)) { video.src = src; play(video); }
        else setStatus(rootOf(video), 'This browser cannot play live video');
        return;
      }
      // A tile is a thumbnail: cap it to the box it is drawn in and hold a
      // short buffer, so a Playground of live tiles costs a fraction of one
      // watch surface. The stage is the opposite — it is the thing being
      // watched, so it takes the whole ladder.
      var hls = new Hls(rec.tile ? {
        lowLatencyMode: false,
        liveSyncDurationCount: 3,
        maxBufferLength: 6,
        backBufferLength: 4,
        enableWorker: true,
        capLevelToPlayerSize: true,
        startLevel: 0
      } : {
        lowLatencyMode: true,
        liveSyncDurationCount: 3,
        backBufferLength: 30,
        enableWorker: true,
        capLevelToPlayerSize: false
      });
      rec.hls = hls;

      var root = rootOf(video);
      hls.on(Hls.Events.MANIFEST_PARSED, function () {
        syncLadder(root, hls.levels || []);
        markActiveRung(root, -1);
        play(video);
      });
      hls.on(Hls.Events.LEVEL_SWITCHED, function (_e, data) {
        if (hls.autoLevelEnabled) markActiveRung(root, -1);
        else markActiveRung(root, data.level);
      });
      hls.on(Hls.Events.MANIFEST_PARSED, function () { rec.parsed = true; });
      hls.on(Hls.Events.ERROR, function (_e, data) {
        if (!data || !data.fatal) return;
        if (attached.get(video) !== rec) return;
        // A fatal decoder error is reported and retried on a widening delay,
        // never swallowed and never hammered. Whether the broadcast is over is
        // the server's word, and it arrives as a Fragment; all this does is
        // keep trying to decode for as long as that is worth doing — and while
        // the server says the broadcast is running, that is always.
        //
        // There is deliberately no point at which this stops trying. It used
        // to give up after six failed fetches, about half a minute, and that
        // half minute was exactly the gap between the server saying live and
        // the first segment reaching disk: a viewer who arrived in it was told
        // "no video is arriving" over a broadcast that then ran for an hour,
        // and nothing in this file ever asked again. A decoder is released by
        // the server replacing this Facet, or by the element leaving the
        // document. Never by its own count.
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
          rec.retries++;
          setStatus(root, rec.parsed
            ? 'Stream interrupted — reconnecting'
            : 'Waiting for video from this broadcast');
          clearTimeout(rec.timer);
          rec.timer = setTimeout(function () {
            if (attached.get(video) !== rec) return;
            try { hls.startLoad(); } catch (_) {}
            play(video);
          }, Math.min(1000 * Math.pow(2, Math.min(rec.retries, 4) - 1), 8000));
        } else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
          rec.retries++;
          if (rec.retries > 3) {
            setStatus(root, 'This stream cannot be decoded here');
            detach(video);
            return;
          }
          setStatus(root, 'Playback error — recovering');
          hls.recoverMediaError();
        } else {
          setStatus(root, 'This stream cannot be played in this browser');
          detach(video);
        }
      });
      hls.loadSource(src);
      hls.attachMedia(video);
    }).catch(function (err) {
      console.error('[live] hls.js unavailable', err);
      if (nativeHls(video)) {
        setStatus(rootOf(video), '');
        video.src = src;
        play(video);
        return;
      }
      setStatus(rootOf(video), 'Video engine failed to load');
      setDecode(host, 'idle');
    });
  }

  // Give the machine back everything this decoder was holding: the hls
  // instance, its worker and its buffers, the element's own buffered media,
  // and any pending retry. The element itself stays exactly as the server
  // rendered it and repaints its poster.
  function detach(video) {
    var rec = attached.get(video);
    if (!rec) return;
    attached.delete(video);

    clearTimeout(rec.timer);
    clearInterval(rec.watch);
    try { video.pause(); } catch (_) {}
    if (rec.hls) {
      try { rec.hls.stopLoad(); } catch (_) {}
      try { rec.hls.detachMedia(); } catch (_) {}
      try { rec.hls.destroy(); } catch (_) {}
      rec.hls = null;
    }
    try {
      video.removeAttribute('src');
      video.srcObject = null;
      video.load();            // drops the element's buffered media, repaints poster
    } catch (_) {}
    setDecode(rec.host, 'idle');
  }

  // Forget an element entirely — it has left the document.
  function release(video) {
    detach(video);
    onScreen.delete(video);
    if (observed.has(video) && tileObserver) {
      tileObserver.unobserve(video);
      observed.delete(video);
    }
  }

  function syncLabels(root, video) {
    var playBtn = root.querySelector('.live-ctl--play');
    if (playBtn) playBtn.setAttribute('aria-label', video.paused ? 'Play' : 'Pause');
    var big = root.querySelector('.live-player__bigplay');
    if (big) big.setAttribute('aria-label', video.paused ? 'Play' : 'Pause');
    var mute = root.querySelector('.live-ctl--mute');
    if (mute) mute.setAttribute('aria-label', video.muted ? 'Unmute' : 'Mute');
    var pill = root.querySelector('.live-player__unmute');
    if (pill) pill.setAttribute('aria-label', video.muted ? 'Turn sound on' : 'Turn sound off');
  }

  function videoOf(root) { return root.querySelector('video[' + STAGE_SRC + ']'); }

  function inFullscreen() {
    return !!(document.fullscreenElement || document.webkitFullscreenElement);
  }

  function toggleFullscreen(root) {
    var video = videoOf(root);
    if (inFullscreen()) {
      (document.exitFullscreen || document.webkitExitFullscreen || function () {}).call(document);
      return;
    }
    if (video && typeof video.webkitEnterFullscreen === 'function' && !root.requestFullscreen) {
      try { video.webkitEnterFullscreen(); } catch (_) {}
      return;
    }
    var req = root.requestFullscreen || root.webkitRequestFullscreen;
    if (req) req.call(root).catch(function () {});
  }

  function onFsChange() {
    document.querySelectorAll('.live-player').forEach(function (root) {
      var on = inFullscreen() && (document.fullscreenElement === root || root.contains(document.fullscreenElement));
      root.setAttribute('data-fs', on ? '1' : '0');
      var btn = root.querySelector('.live-ctl--fs');
      if (btn) btn.setAttribute('aria-label', on ? 'Minimize' : 'Fullscreen');
    });
  }

  // ── viewport gating for Playground tiles ───────────────────────────────────

  var tileObserver = ('IntersectionObserver' in window) ? new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      var v = e.target;
      if (e.isIntersecting) {
        onScreen.add(v);
        if (!document.hidden) { attach(v); play(v); }
      } else {
        onScreen.delete(v);
        detach(v);
      }
    });
  }, { rootMargin: '200px 0px', threshold: 0.2 }) : null;

  function mount(scope) {
    if (!scope || !scope.querySelectorAll) return;
    if (scope.matches && scope.matches(DECODERS)) mountOne(scope);
    scope.querySelectorAll(DECODERS).forEach(mountOne);
  }

  function mountOne(video) {
    if (isTile(video)) {
      if (!tileObserver) { attach(video); play(video); return; } // no observer: decode it
      if (observed.has(video)) return;
      observed.add(video);
      tileObserver.observe(video);
      return;
    }
    // The stage is why the page was opened. It decodes immediately.
    attach(video);
  }

  // ── delegated actions ──────────────────────────────────────────────────────

  function dispatch(action, el, ev) {
    var root = rootOf(el);
    var video = root ? videoOf(root) : null;

    switch (action) {
      case 'back':
        var href = el.getAttribute('data-live-href');
        var go = (window.F33D3R && window.F33D3R.visit) ? window.F33D3R.visit : function (u) { location.href = u; };
        if (href) { go(href); } else if (history.length > 1) { history.back(); } else { go('/'); }
        return;

      case 'playpause':
        if (!video) return;
        if (video.paused) {
          setStatus(root, '');
          play(video);
        } else {
          video.pause();
        }
        return;

      case 'mute':
        if (!video) return;
        video.muted = !video.muted;
        // A muted stage may have been decoding all along; a viewer asking for
        // sound is also asking for it to be running.
        if (video.paused) play(video);
        if (ev) ev.preventDefault();
        return;

      case 'fullscreen':
        if (root) toggleFullscreen(root);
        return;

      case 'quality-toggle':
        if (!root) return;
        var q = root.querySelector('.live-quality');
        if (q) openQuality(root, q.getAttribute('data-open') !== '1');
        return;

      case 'quality-set':
        if (!root || !video) return;
        var recq = attached.get(video);
        var lvl = parseInt(el.getAttribute('data-level'), 10);
        if (recq && recq.hls && !isNaN(lvl)) {
          recq.hls.currentLevel = lvl;      // -1 restores automatic selection
          markActiveRung(root, lvl);
        }
        openQuality(root, false);
        return;

      case 'copy-link':
        var url = el.getAttribute('data-live-url') || '';
        if (url && url.charAt(0) === '/') url = location.origin + url;
        if (navigator.clipboard && url) {
          navigator.clipboard.writeText(url).then(function () {
            if (window.toast) window.toast('Link copied');
          }).catch(function () {
            if (window.toast) window.toast('Could not copy link', 'error');
          });
        }
        if (ev) ev.preventDefault();
        return;
    }
  }

  function init() {
    mount(document);

    document.addEventListener('click', function (e) {
      var stop = e.target.closest ? e.target.closest('[data-live-stop]') : null;
      if (stop) e.stopPropagation();
      var el = e.target.closest ? e.target.closest('[data-live-action]') : null;
      if (!el) {
        // Clicking outside an open quality menu closes it.
        document.querySelectorAll('.live-quality[data-open="1"]').forEach(function (open) {
          var root = rootOf(open);
          if (root && !open.contains(e.target)) openQuality(root, false);
        });
        return;
      }
      dispatch(el.getAttribute('data-live-action'), el, e);
    });

    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape') return;
      var open = document.querySelector('.live-quality[data-open="1"]');
      if (!open) return;
      var oroot = rootOf(open);
      if (oroot) { openQuality(oroot, false); e.preventDefault(); }
    });

    document.addEventListener('fullscreenchange', onFsChange);
    document.addEventListener('webkitfullscreenchange', onFsChange);

    // A hidden tab decodes nothing. The tiles the viewport still holds are
    // remembered so they come straight back when the tab does.
    document.addEventListener('visibilitychange', function () {
      onScreen.forEach(function (v) {
        if (document.hidden) { detach(v); }
        else if (v.isConnected) { attach(v); play(v); }
      });
    });

    // Every Fragment the server delivers arrives as a DOM change, whatever
    // carried it — htmx swap, FA Live push, or the heartbeat retargeting the
    // player onto its terminal state. One rule covers all three: a decoder
    // that leaves the document is destroyed, and markup that enters it is
    // mounted.
    new MutationObserver(function (records) {
      for (var i = 0; i < records.length; i++) {
        var rem = records[i].removedNodes;
        for (var j = 0; j < rem.length; j++) {
          var n = rem[j];
          if (n.nodeType !== 1) continue;
          if (n.matches && n.matches(DECODERS)) release(n);
          if (n.querySelectorAll) n.querySelectorAll(DECODERS).forEach(release);
        }
        var add = records[i].addedNodes;
        for (var k = 0; k < add.length; k++) {
          if (add[k].nodeType === 1) mount(add[k]);
        }
      }
    }).observe(document.documentElement, { childList: true, subtree: true });

    // A page leaving for good releases its decoders too, so a back/forward
    // restore never finds a half-torn worker still holding memory.
    window.addEventListener('pagehide', function () {
      document.querySelectorAll(DECODERS).forEach(detach);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  window.F33D3R_LivePlayer = { attach: attach, detach: detach, mount: mount };
})();
