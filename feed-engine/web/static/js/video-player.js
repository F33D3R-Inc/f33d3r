// F33D3R native HLS video player — v20260505
//
// - hls.js served locally (/static/js/hls.min.js) — no CDN, no CSP issues
// - Feed videos: poster shown, play on click only (no autoplay drain)
// - Viewport observer: pause when scrolled away, dispose when removed from DOM
// - Only one hls.js instance active at a time in the feed

(function () {
    'use strict';

    if (window.__F33D3R_VIDEO_LOADED__) return;
    window.__F33D3R_VIDEO_LOADED__ = true;

    var HLS_LOCAL = '/static/js/hls.min.js';
    var hlsLoading = null;
    var attachedPlayers = new WeakMap();

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
            s.onload = function () { resolve(window.Hls); };
            s.onerror = function () { reject(new Error('hls.js load failed')); };
            document.head.appendChild(s);
        });
        return hlsLoading;
    }

    function attachHls(video, src) {
        if (nativeHls(video)) {
            video.src = src;
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
                autoStartLoad: false,   // don't fetch segments until play
            });
            hls.loadSource(src);
            hls.attachMedia(video);
            video.addEventListener('play', function onPlay() {
                hls.startLoad();
                video.removeEventListener('play', onPlay);
            }, { once: true });
            return hls;
        }).catch(function (err) {
            console.warn('[f33d3r video] hls.js init failed:', err);
            video.src = src;
            return null;
        });
    }

    function disposePlayer(video) {
        var rec = attachedPlayers.get(video);
        if (!rec) return;
        try { if (rec.hls) rec.hls.destroy(); } catch (_) {}
        try { if (rec.observer) rec.observer.disconnect(); } catch (_) {}
        try { video.pause(); video.removeAttribute('src'); video.load(); } catch (_) {}
        attachedPlayers.delete(video);
    }

    function makeViewportObserver(video) {
        var io = new IntersectionObserver(function (entries) {
            entries.forEach(function (e) {
                if (!e.isIntersecting && !video.paused) {
                    video.pause();
                }
            });
        }, { rootMargin: '100px 0px', threshold: 0.0 });
        io.observe(video);
        return io;
    }

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

    function attachOne(video) {
        if (!video || !(video instanceof HTMLVideoElement)) return;
        if (attachedPlayers.has(video)) return;
        var src = video.dataset.f33dHls || video.getAttribute('data-hls') || '';
        if (!src) return;

        // Feed videos: no autoplay, show poster, user must click play
        video.preload = 'none';
        video.muted   = false;         // unmuted — user chose to play
        video.setAttribute('playsinline', '');
        video.controls = true;

        var hlsRet = attachHls(video, src);
        var observer = makeViewportObserver(video);
        Promise.resolve(hlsRet).then(function (h) {
            attachedPlayers.set(video, { hls: h || null, observer: observer });
        });
    }

    function attachAll(root) {
        var nodes = (root || document).querySelectorAll('video[data-f33d-hls]');
        nodes.forEach(attachOne);
    }

    function init() {
        ensureDomObserver();
        attachAll(document);
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
        attach: attachOne,
        attachAll: attachAll,
        dispose: disposePlayer,
    };
})();
