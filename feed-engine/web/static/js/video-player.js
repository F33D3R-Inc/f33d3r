// F33D3R video player — v20260513c
//
// Smart source detection: .m3u8 → hls.js (or native Safari HLS),
//   .mp4/.mov/.webm → plain video.src (fixes Firefox + legacy uploads)
// X-style custom overlay controls on desktop (.pcd), tap-to-play on mobile (.pcf)
// IntersectionObserver autoplay in feed; max 3 active hls.js instances

(function () {
    'use strict';

    if (window.__F33D3R_VIDEO_LOADED__) return;
    window.__F33D3R_VIDEO_LOADED__ = true;

    var HLS_LOCAL = '/static/js/hls.min.js';
    var hlsLoading = null;
    // WeakMap<video, {hls, observer, ctrlObserver, wrap, fadeTimer}>
    var attachedPlayers = new WeakMap();
    // LRU queue of active hls.js instances — max 3
    var activeHlsQueue = [];
    var MAX_HLS = 3;
    // Flat array of all attached video elements — used by scroll idle detection
    // (WeakMap has no forEach, so we maintain this in parallel)
    var attachedVideosList = [];
    // The one video that is currently playing with audio (global singleton)
    var activePlayer = null;

    // True on Safari/iOS — native HLS means video.src is set directly and
    // play() must be deferred until the canplay event fires, not called
    // immediately when the IntersectionObserver threshold is crossed.
    var supportsNativeHLS = (function () {
        var v = document.createElement('video');
        return !!v.canPlayType('application/vnd.apple.mpegurl');
    })();

    // ── helpers ────────────────────────────────────────────────────────────────

    function isHlsUrl(src) {
        return src && src.indexOf('.m3u8') !== -1;
    }

    function nativeHls(video) {
        return video.canPlayType('application/vnd.apple.mpegurl') !== '';
    }

    function ensureHlsLib() {
        if (window.Hls) return Promise.resolve(window.Hls);
        if (hlsLoading) return hlsLoading;
        hlsLoading = new Promise(function (resolve, reject) {
            var s = document.createElement('script');
            s.src = HLS_LOCAL;
            s.async = true;
            s.onload  = function () { resolve(window.Hls); };
            s.onerror = function () { reject(new Error('hls.js load failed')); };
            document.head.appendChild(s);
        });
        return hlsLoading;
    }

    function trackHlsInstance(hls) {
        if (!hls) return;
        activeHlsQueue.push(hls);
        if (activeHlsQueue.length > MAX_HLS) {
            var oldest = activeHlsQueue.shift();
            try { oldest.destroy(); } catch (_) {}
        }
    }

    function removeHlsFromQueue(hls) {
        if (!hls) return;
        var idx = activeHlsQueue.indexOf(hls);
        if (idx !== -1) activeHlsQueue.splice(idx, 1);
    }

    // Attach HLS or plain src depending on URL type.
    // Returns Promise<hls|null>
    function attachSource(video, src) {
        if (!isHlsUrl(src)) {
            // Plain mp4/mov/webm — direct src assignment, no hls.js
            video.src = src;
            return Promise.resolve(null);
        }
        // HLS path
        if (nativeHls(video)) {
            video.src = src;
            video.load(); // Safari requires explicit load() for canplay to fire
            return Promise.resolve(null);
        }
        return ensureHlsLib().then(function (Hls) {
            if (!Hls || !Hls.isSupported()) {
                video.src = src;
                return null;
            }
            var hls = new Hls({
                maxBufferLength: 10,
                maxMaxBufferLength: 20,
                maxBufferSize: 10 * 1024 * 1024,
                lowLatencyMode: false,
                enableWorker: true,
                startFragPrefetch: false,
                capLevelToPlayerSize: true,
                autoStartLoad: false,  // load segments only when play() is called
            });
            hls.loadSource(src);
            hls.attachMedia(video);
            // Do NOT trackHlsInstance here — the LRU queue only counts
            // instances that are actively playing, not idle attached ones.
            // trackHlsInstance is called in playAsActive() instead.
            return hls;
        }).catch(function (err) {
            console.warn('[f33d3r video] hls.js init failed:', err);
            video.src = src;
            return null;
        });
    }

    // ── dispose ────────────────────────────────────────────────────────────────

    function disposePlayer(video) {
        var rec = attachedPlayers.get(video);
        if (!rec) return;
        if (rec.hls) { removeHlsFromQueue(rec.hls); try { rec.hls.destroy(); } catch (_) {} }
        try { if (rec.observer) rec.observer.disconnect(); } catch (_) {}
        try { if (rec.ctrlObserver) rec.ctrlObserver.disconnect(); } catch (_) {}
        try { if (rec.fadeTimer) clearTimeout(rec.fadeTimer); } catch (_) {}
        try { video.pause(); video.removeAttribute('src'); video.load(); } catch (_) {}
        if (activePlayer === video) activePlayer = null;
        var listIdx = attachedVideosList.indexOf(video);
        if (listIdx !== -1) attachedVideosList.splice(listIdx, 1);
        attachedPlayers.delete(video);
    }

    // ── custom overlay controls (desktop .pcd only) ───────────────────────────

    // seconds → "m:ss"
    function fmtTime(s) {
        if (!isFinite(s) || s < 0) return '0:00';
        var m = Math.floor(s / 60);
        var sec = Math.floor(s % 60);
        return m + ':' + (sec < 10 ? '0' : '') + sec;
    }

    // Icons as inline SVG strings
    var SVG = {
        play: '<svg viewBox="0 0 24 24" fill="currentColor"><polygon points="5,3 19,12 5,21"/></svg>',
        pause: '<svg viewBox="0 0 24 24" fill="currentColor"><rect x="5" y="3" width="4" height="18" rx="1"/><rect x="15" y="3" width="4" height="18" rx="1"/></svg>',
        muted: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M11 5L6 9H2v6h4l5 4V5zM23 9l-6 6M17 9l6 6" stroke="currentColor" stroke-width="2" fill="none"/></svg>',
        unmuted: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M11 5L6 9H2v6h4l5 4V5z"/><path d="M15.54 8.46a5 5 0 0 1 0 7.07M19.07 4.93a10 10 0 0 1 0 14.14" stroke="currentColor" stroke-width="2" fill="none"/></svg>',
        fullscreen: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 3H5a2 2 0 00-2 2v3m18 0V5a2 2 0 00-2-2h-3m0 18h3a2 2 0 002-2v-3M3 16v3a2 2 0 002 2h3"/></svg>',
        exitFs:     '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 3v3a2 2 0 01-2 2H3m18 0h-3a2 2 0 01-2-2V3m0 18v-3a2 2 0 012-2h3M3 16h3a2 2 0 012 2v3"/></svg>',
        pip: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="2" y="3" width="20" height="18" rx="2"/><rect x="12" y="13" width="8" height="6" rx="1"/></svg>',
    };

    function buildOverlay(video, wrap, authorHandle) {
        // Outer overlay container
        var ov = document.createElement('div');
        ov.className = 'vp-overlay';

        // Central big play button (visible when paused)
        var bigPlay = document.createElement('button');
        bigPlay.className = 'vp-big-play';
        bigPlay.type = 'button';
        bigPlay.innerHTML = SVG.play;
        bigPlay.setAttribute('aria-label', 'Play');

        // Bottom control bar
        var bar = document.createElement('div');
        bar.className = 'vp-bar';

        // Scrubber row
        var scrubRow = document.createElement('div');
        scrubRow.className = 'vp-scrub-row';
        var scrub = document.createElement('input');
        scrub.type = 'range';
        scrub.className = 'vp-scrub';
        scrub.min = '0';
        scrub.max = '100';
        scrub.step = '0.1';
        scrub.value = '0';
        scrub.setAttribute('aria-label', 'Seek');
        scrubRow.appendChild(scrub);

        // Bottom buttons row
        var btnRow = document.createElement('div');
        btnRow.className = 'vp-btn-row';

        var timeEl = document.createElement('span');
        timeEl.className = 'vp-time';
        timeEl.textContent = '0:00 / 0:00';

        var muteBtn = document.createElement('button');
        muteBtn.type = 'button';
        muteBtn.className = 'vp-btn';
        muteBtn.innerHTML = SVG.muted;
        muteBtn.setAttribute('aria-label', 'Unmute');

        var spacer = document.createElement('span');
        spacer.style.flex = '1';

        var fsBtn = document.createElement('button');
        fsBtn.type = 'button';
        fsBtn.className = 'vp-btn';
        fsBtn.innerHTML = SVG.fullscreen;
        fsBtn.setAttribute('aria-label', 'Fullscreen');

        btnRow.appendChild(timeEl);
        btnRow.appendChild(muteBtn);
        btnRow.appendChild(spacer);

        // PiP button only if supported
        if (document.pictureInPictureEnabled) {
            var pipBtn = document.createElement('button');
            pipBtn.type = 'button';
            pipBtn.className = 'vp-btn';
            pipBtn.innerHTML = SVG.pip;
            pipBtn.setAttribute('aria-label', 'Picture in Picture');
            pipBtn.addEventListener('click', function (e) {
                e.stopPropagation();
                if (document.pictureInPictureElement) {
                    document.exitPictureInPicture();
                } else {
                    video.requestPictureInPicture().catch(function () {});
                }
            });
            btnRow.appendChild(pipBtn);
        }

        btnRow.appendChild(fsBtn);

        bar.appendChild(scrubRow);
        bar.appendChild(btnRow);

        ov.appendChild(bigPlay);
        ov.appendChild(bar);

        wrap.appendChild(ov);
        wrap.style.position = 'relative';

        // ── control visibility state machine ──
        // States: 'hidden', 'visible', 'fading'
        var visTimer = null;

        function showControls() {
            if (visTimer) clearTimeout(visTimer);
            ov.classList.add('vp-ctrl-visible');
            ov.classList.remove('vp-ctrl-fading');
            visTimer = setTimeout(function () {
                if (!video.paused) fadeControls();
            }, 3000);
        }

        function fadeControls() {
            ov.classList.remove('vp-ctrl-visible');
            ov.classList.add('vp-ctrl-fading');
        }

        function hideControls() {
            if (visTimer) { clearTimeout(visTimer); visTimer = null; }
            ov.classList.remove('vp-ctrl-visible', 'vp-ctrl-fading');
        }

        // ── interactions ──
        ov.addEventListener('mouseenter', showControls);
        ov.addEventListener('mousemove', showControls);
        ov.addEventListener('mouseleave', function () {
            if (!video.paused) fadeControls();
        });

        // Touch: tap to toggle controls; don't steal video-level tap-to-play
        ov.addEventListener('touchstart', function (e) {
            // Only show/hide controls on the overlay itself, not on specific buttons
            if (e.target === ov || e.target === bigPlay || e.target.closest('.vp-bar')) {
                if (ov.classList.contains('vp-ctrl-visible')) {
                    if (!video.paused) fadeControls();
                } else {
                    showControls();
                }
            }
        }, { passive: true });

        // Big central play/pause
        bigPlay.addEventListener('click', function (e) {
            e.stopPropagation();
            togglePlay();
        });

        // Clicking the video area (not on a bar button) also toggles play
        ov.addEventListener('click', function (e) {
            if (e.target === ov) togglePlay();
        });

        function togglePlay() {
            if (video.paused) {
                playAsActive(video);
            } else {
                video.pause();
            }
        }

        // Mute button
        muteBtn.addEventListener('click', function (e) {
            e.stopPropagation();
            video.muted = !video.muted;
            updateMuteBtn();
            showControls();
        });

        function updateMuteBtn() {
            muteBtn.innerHTML = video.muted ? SVG.muted : SVG.unmuted;
            muteBtn.setAttribute('aria-label', video.muted ? 'Unmute' : 'Mute');
        }

        // Fullscreen — three-path: iOS Safari → Android/Chrome → desktop
        fsBtn.addEventListener('click', function (e) {
            e.stopPropagation();

            // ── iOS Safari: standard requestFullscreen() is not supported.
            // webkitEnterFullscreen() must be called on the <video> element itself.
            if (typeof video.webkitEnterFullscreen === 'function') {
                if (video.webkitDisplayingFullscreen) {
                    video.webkitExitFullscreen();
                } else {
                    // webkitEnterFullscreen requires the video to be playing or paused
                    // with a valid src — it will throw if called on an unloaded video.
                    try { video.webkitEnterFullscreen(); } catch (err) {}
                }
                showControls();
                return;
            }

            // ── Standard Fullscreen API (Android Chrome, Firefox, desktop) ──
            var inFs = document.fullscreenElement || document.webkitFullscreenElement;
            if (inFs) {
                (document.exitFullscreen || document.webkitExitFullscreen || function(){}).call(document);
            } else {
                // Always request fullscreen on the wrap so the controls overlay (vp-overlay,
                // vp-bar, video-bottom-pills) stays visible in fullscreen.
                // Requesting on the video element alone leaves the overlay behind in the DOM.
                var wrapReq = wrap.requestFullscreen || wrap.webkitRequestFullscreen;
                if (wrapReq) {
                    wrapReq.call(wrap).catch(function () {
                        // Last resort: bare video fullscreen (no overlay controls)
                        var req = video.requestFullscreen || video.webkitRequestFullscreen;
                        if (req) req.call(video).catch(function () {});
                    });
                }
            }
            showControls();
        });

        // Keep fullscreen button icon in sync with actual fullscreen state
        function onFsChange() {
            var inFs = document.fullscreenElement || document.webkitFullscreenElement
                       || video.webkitDisplayingFullscreen;
            fsBtn.innerHTML = inFs ? SVG.exitFs : SVG.fullscreen;
            fsBtn.setAttribute('aria-label', inFs ? 'Exit fullscreen' : 'Fullscreen');
        }
        document.addEventListener('fullscreenchange', onFsChange);
        document.addEventListener('webkitfullscreenchange', onFsChange);
        video.addEventListener('webkitbeginfullscreen', function() {
            onFsChange();
            // iOS element-level fullscreen hides everything outside <video>.
            // Flash the lineage chip text briefly so creator credit is seen.
            var chip = wrap && wrap.querySelector('.lineage-chip');
            if (chip) {
                var flash = document.createElement('div');
                flash.className = 'vp-ios-lineage-flash';
                flash.textContent = chip.textContent;
                document.body.appendChild(flash);
                setTimeout(function() { flash.remove(); }, 3000);
            }
        });
        video.addEventListener('webkitendfullscreen', onFsChange);

        // Scrubber
        var isScrubbing = false;
        scrub.addEventListener('mousedown', function () { isScrubbing = true; });
        scrub.addEventListener('touchstart', function () { isScrubbing = true; }, { passive: true });
        window.addEventListener('mouseup', function () { isScrubbing = false; });
        window.addEventListener('touchend', function () { isScrubbing = false; });

        scrub.addEventListener('input', function (e) {
            e.stopPropagation();
            if (video.duration) {
                video.currentTime = (scrub.value / 100) * video.duration;
            }
            showControls();
        });

        // ── video event listeners ──
        video.addEventListener('timeupdate', function () {
            if (!isScrubbing && video.duration) {
                scrub.value = (video.currentTime / video.duration) * 100;
            }
            timeEl.textContent = fmtTime(video.currentTime) + ' / ' + fmtTime(video.duration);
        });

        video.addEventListener('durationchange', function () {
            timeEl.textContent = fmtTime(video.currentTime) + ' / ' + fmtTime(video.duration);
        });

        video.addEventListener('play', function () {
            bigPlay.classList.add('vp-big-play-hidden');
            updateMuteBtn();
            showControls();
        });

        video.addEventListener('pause', function () {
            bigPlay.classList.remove('vp-big-play-hidden');
            showControls();
        });

        video.addEventListener('ended', function () {
            bigPlay.classList.remove('vp-big-play-hidden');
            hideControls();
        });

        // Initial state: show big play, controls hidden
        updateMuteBtn();

        return { showControls: showControls, hideControls: hideControls, visTimer: visTimer };
    }

    // ── autoplay / active-player tracking ─────────────────────────────────────

    // Play a video as the sole active player (pauses previous).
    // Uses canplay fallback so Firefox (strict play() rejection) works correctly.
    function playAsActive(video) {
        if (activePlayer && activePlayer !== video && !activePlayer.paused) {
            activePlayer.pause();
        }
        activePlayer = video;
        var rec = attachedPlayers.get(video);
        if (rec && rec.hls) {
            // Only enter LRU queue when actually playing — not at attach time.
            // This prevents idle feed videos from evicting each other on load.
            trackHlsInstance(rec.hls);
            rec.hls.startLoad(-1);
        }
        video.play().catch(function () {
            // Firefox rejects play() if media isn't buffered yet.
            // Wait for canplay then retry — only if this is still the active player.
            video.addEventListener('canplay', function onCanPlay() {
                if (activePlayer === video) video.play().catch(function () {});
            }, { once: true });
        });
    }

    // ── viewport observer (autoplay for feed) ─────────────────────────────────

    function makeViewportObserver(video, isMobile) {
        var io = new IntersectionObserver(function (entries) {
            entries.forEach(function (e) {
                var rec = attachedPlayers.get(video);
                if (e.isIntersecting && e.intersectionRatio >= 0.5) {
                    // Autoplay muted when 50%+ visible — X.com standard threshold.
                    // Scroll idle detection handles flicker prevention instead.
                    if (video.paused) {
                        video.muted = true;
                        if (rec && rec.hls) {
                            // hls.js is ready — play now
                            playAsActive(video);
                        } else if (rec && supportsNativeHLS) {
                            // Safari native HLS: src is set but canplay hasn't fired yet.
                            // Mark pending and let the canplay listener trigger play() —
                            // calling play() before canplay puts Safari in an error state
                            // that blocks all subsequent play() calls until user clicks.
                            rec.pendingAutoplay = true;
                            if (!rec.canplayAttached) {
                                rec.canplayAttached = true;
                                video.addEventListener('canplay', function onCanplay() {
                                    video.removeEventListener('canplay', onCanplay);
                                    if (rec.pendingAutoplay && video.paused) {
                                        playAsActive(video);
                                    }
                                }, { once: true });
                            }
                        } else if (rec) {
                            // hls.js still initialising — mark pending; .then() will fire
                            rec.pendingAutoplay = true;
                        }
                    }
                } else if (!e.isIntersecting || e.intersectionRatio < 0.5) {
                    if (!video.paused) video.pause();
                    if (!isMobile) video.currentTime = 0;
                    // Cancel any pending autoplay — user scrolled past
                    if (rec) rec.pendingAutoplay = false;
                }
            });
        }, { threshold: [0, 0.5], rootMargin: '0px 0px -10% 0px' });
        io.observe(video);
        return io;
    }

    // ── DOM mutation observer (cleanup on node removal) ───────────────────────

    var domObs;
    function ensureDomObserver() {
        if (domObs) return;
        domObs = new MutationObserver(function (records) {
            records.forEach(function (r) {
                r.removedNodes.forEach(function (n) {
                    if (!(n instanceof HTMLElement)) return;
                    var vids = n.tagName === 'VIDEO'
                        ? [n]
                        : Array.from(n.querySelectorAll('video[data-f33d-hls]'));
                    vids.forEach(disposePlayer);
                });
            });
        });
        domObs.observe(document.body, { childList: true, subtree: true });
    }

    // ── attachOne ─────────────────────────────────────────────────────────────

    function attachOne(video) {
        if (!video || !(video instanceof HTMLVideoElement)) return;
        if (attachedPlayers.has(video)) return;
        var src = video.dataset.f33dHls || video.getAttribute('data-hls') || '';
        if (!src) return;

        video.setAttribute('playsinline', '');
        video.setAttribute('webkit-playsinline', '');  // iOS < 10 inline playback
        video.preload = 'metadata';  // Firefox needs metadata before play() resolves
        video.muted   = true;  // always start muted for autoplay compatibility

        // Detect layout context
        var isMobileCard = !!video.closest('.pcf');
        var isDesktopCard = !!video.closest('.pcd');

        // All cards loop while in viewport
        video.loop = true;

        if (isMobileCard) {
            // Mobile focus card: muted loop autoplay, tap to play/pause
            video.muted = true;
            video.removeAttribute('controls');
            video.style.cursor = 'pointer';
            video.addEventListener('click', function (e) {
                e.stopPropagation();
                if (video.paused) playAsActive(video); else video.pause();
            });
        } else if (isDesktopCard) {
            // Desktop feed card: custom overlay controls, no native controls
            video.removeAttribute('controls');
            var wrap = video.parentElement;
            var authorHandle = video.dataset.mediaCreator || '';
            if (!authorHandle) {
                var article = video.closest('article');
                if (article) {
                    var handleLink = article.querySelector('.post-handle');
                    if (handleLink) authorHandle = handleLink.textContent.replace('@', '').trim();
                }
            }
            buildOverlay(video, wrap, authorHandle);
        } else {
            // Fallback: treat like desktop card — custom overlay, no native controls
            video.removeAttribute('controls');
            var wrap = video.parentElement;
            var authorHandle = video.dataset.mediaCreator || '';
            buildOverlay(video, wrap, authorHandle);
        }

        var observer = makeViewportObserver(video, isMobileCard);

        var rec = { hls: null, observer: observer, pendingAutoplay: false };
        attachedPlayers.set(video, rec);
        attachedVideosList.push(video);

        attachSource(video, src).then(function (hls) {
            rec.hls = hls || null;
            if (hls && !video.paused) {
                // Video started playing before hls was ready (native HLS / plain src)
                hls.startLoad();
            } else if (rec.pendingAutoplay && video.paused && !supportsNativeHLS) {
                // IO fired before hls.js was ready — fire deferred play now.
                // Native HLS (Safari) is excluded: the canplay listener in
                // makeViewportObserver is the sole play trigger for that path.
                playAsActive(video);
            }
        });
    }

    function attachAll(root) {
        var nodes = (root || document).querySelectorAll('video[data-f33d-hls]');
        nodes.forEach(attachOne);
    }

    // ── init ──────────────────────────────────────────────────────────────────

    function init() {
        ensureDomObserver();
        attachAll(document);

        // Pause active player before HTMX swaps DOM — prevents audio bleed
        // when navigating to a new panel.
        document.body.addEventListener('htmx:beforeSwap', function () {
            if (activePlayer && !activePlayer.paused) {
                activePlayer.pause();
            }
        });

        document.body.addEventListener('htmx:afterSwap', function (e) {
            attachAll(e.detail && e.detail.target ? e.detail.target : document);
        });

        document.addEventListener('visibilitychange', function () {
            if (document.hidden) {
                document.querySelectorAll('video[data-f33d-hls]').forEach(function (v) {
                    if (!v.paused) v.pause();
                });
            }
        });

        // Scroll idle detection — pause while scrolling, let IO re-trigger after idle.
        // Matches X.com behavior: no flicker during fast scrolling.
        //
        // Safari and Firefox fire scroll events on the actual scrolling element, not on
        // window, when a positioned element has overflow-y:scroll. The focus-feed layout
        // (#main-col) is exactly that. We attach to both window AND the focus-feed
        // container so the logic fires consistently on all browsers.
        var _scrollPauseTimer = null;
        function _onScroll() {
            if (activePlayer && !activePlayer.paused) {
                activePlayer.pause();
            }
            clearTimeout(_scrollPauseTimer);
            _scrollPauseTimer = setTimeout(function () {
                _scrollPauseTimer = null;
                attachedVideosList.forEach(function (video) {
                    var rec = attachedPlayers.get(video);
                    if (rec && rec.observer) {
                        rec.observer.unobserve(video);
                        rec.observer.observe(video);
                    }
                });
            }, 150);
        }
        window.addEventListener('scroll', _onScroll, { passive: true });
        // Also target the focus-feed scroll container — present on the main feed page.
        // Use a MutationObserver-free approach: query now and after DOMContentLoaded.
        function _attachFeedScroller() {
            var feedEl = document.getElementById('main-col') ||
                         document.querySelector('.f33d3r-main.focus-feed');
            if (feedEl && !feedEl.__f33dScrollAttached) {
                feedEl.__f33dScrollAttached = true;
                feedEl.addEventListener('scroll', _onScroll, { passive: true });
            }
        }
        _attachFeedScroller();
        document.addEventListener('DOMContentLoaded', _attachFeedScroller);

        window.addEventListener('pagehide', function () {
            try {
                if (window.__F33D3R_SSE__ && typeof window.__F33D3R_SSE__.close === 'function') {
                    window.__F33D3R_SSE__.close();
                }
            } catch (_) {}
        });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }

    window.F33D3R_Video = {
        attach:    attachOne,
        attachAll: attachAll,
        dispose:   disposePlayer,
    };
})();
