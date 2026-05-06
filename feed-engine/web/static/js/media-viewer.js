// F33D3R Media Viewer — X/Twitter-style lightbox
// Handles photos (single + carousel) and HLS video full-screen.
// Intercepts clicks on .post-media-img and .post-media-video elements.

(function () {
    'use strict';

    if (window.__F33D3R_VIEWER_LOADED__) return;
    window.__F33D3R_VIEWER_LOADED__ = true;

    // ── DOM scaffold ─────────────────────────────────────────────────────────────
    var overlay, imgEl, videoEl, prevBtn, nextBtn, closeBtn, counterEl, actionsEl;

    function buildOverlay() {
        if (overlay) return;

        overlay = document.createElement('div');
        overlay.id = 'fv-overlay';
        overlay.style.cssText = [
            'position:fixed;inset:0;z-index:9999',
            'background:rgba(0,0,0,.92)',
            'display:none;flex-direction:column',
            'align-items:center;justify-content:center',
            'user-select:none;-webkit-user-select:none',
        ].join(';');

        // Close on backdrop click (but not on children)
        overlay.addEventListener('click', function (e) {
            if (e.target === overlay) close();
        });

        // Top bar: counter + close
        var topBar = document.createElement('div');
        topBar.style.cssText = 'position:absolute;top:0;left:0;right:0;height:52px;display:flex;align-items:center;justify-content:space-between;padding:0 16px;z-index:2';

        counterEl = document.createElement('span');
        counterEl.style.cssText = 'font-size:13px;font-weight:600;color:rgba(255,255,255,.8);font-family:"DM Sans",sans-serif';

        closeBtn = document.createElement('button');
        closeBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" style="width:20px;height:20px"><path d="M18 6 6 18M6 6l12 12"/></svg>';
        closeBtn.style.cssText = 'width:40px;height:40px;border-radius:50%;border:none;background:rgba(255,255,255,.12);color:#fff;cursor:pointer;display:flex;align-items:center;justify-content:center;transition:background 120ms;margin-left:auto';
        closeBtn.addEventListener('mouseover', function () { this.style.background = 'rgba(255,255,255,.22)'; });
        closeBtn.addEventListener('mouseout',  function () { this.style.background = 'rgba(255,255,255,.12)'; });
        closeBtn.addEventListener('click', close);

        topBar.appendChild(counterEl);
        topBar.appendChild(closeBtn);
        overlay.appendChild(topBar);

        // Media container
        var mediaWrap = document.createElement('div');
        mediaWrap.style.cssText = 'flex:1;width:100%;display:flex;align-items:center;justify-content:center;position:relative;overflow:hidden;padding:60px 60px 80px';

        imgEl = document.createElement('img');
        imgEl.style.cssText = 'max-width:100%;max-height:100%;object-fit:contain;border-radius:4px;display:none';
        mediaWrap.appendChild(imgEl);

        videoEl = document.createElement('video');
        videoEl.style.cssText = 'max-width:100%;max-height:100%;object-fit:contain;border-radius:4px;display:none;outline:none';
        videoEl.controls = true;
        videoEl.setAttribute('playsinline', '');
        mediaWrap.appendChild(videoEl);

        // Prev / Next arrows
        prevBtn = _navBtn('left');
        nextBtn = _navBtn('right');
        mediaWrap.appendChild(prevBtn);
        mediaWrap.appendChild(nextBtn);

        overlay.appendChild(mediaWrap);

        // Bottom action bar
        actionsEl = document.createElement('div');
        actionsEl.style.cssText = 'position:absolute;bottom:0;left:0;right:0;height:68px;display:flex;align-items:center;justify-content:center;gap:32px;background:linear-gradient(to top,rgba(0,0,0,.7),transparent);padding:0 20px';
        overlay.appendChild(actionsEl);

        document.body.appendChild(overlay);

        // Keyboard nav
        document.addEventListener('keydown', function (e) {
            if (!overlay || overlay.style.display === 'none') return;
            if (e.key === 'Escape')      close();
            if (e.key === 'ArrowLeft')   navigate(-1);
            if (e.key === 'ArrowRight')  navigate(1);
        });
    }

    function _navBtn(dir) {
        var btn = document.createElement('button');
        var isLeft = dir === 'left';
        btn.innerHTML = isLeft
            ? '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" style="width:20px;height:20px"><polyline points="15 18 9 12 15 6"/></svg>'
            : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" style="width:20px;height:20px"><polyline points="9 6 15 12 9 18"/></svg>';
        btn.style.cssText = [
            'position:absolute;top:50%;transform:translateY(-50%)',
            isLeft ? 'left:8px' : 'right:8px',
            'width:40px;height:40px;border-radius:50%',
            'background:rgba(255,255,255,.15);border:none;color:#fff',
            'cursor:pointer;display:none;align-items:center;justify-content:center',
            'transition:background 120ms;z-index:3',
        ].join(';');
        btn.addEventListener('mouseover', function () { this.style.background = 'rgba(255,255,255,.28)'; });
        btn.addEventListener('mouseout',  function () { this.style.background = 'rgba(255,255,255,.15)'; });
        btn.addEventListener('click', function (e) {
            e.stopPropagation();
            navigate(isLeft ? -1 : 1);
        });
        return btn;
    }

    // ── State ─────────────────────────────────────────────────────────────────────
    var state = { urls: [], idx: 0, type: 'image', postCard: null, hlsInst: null };

    // ── Open ──────────────────────────────────────────────────────────────────────
    function openImages(urls, startIdx, postCard) {
        buildOverlay();
        state.urls    = urls;
        state.idx     = startIdx || 0;
        state.type    = 'image';
        state.postCard = postCard || null;
        _showImage(state.idx);
        _renderActions();
        overlay.style.display = 'flex';
        document.body.style.overflow = 'hidden';
    }

    function openVideo(src, poster, postCard) {
        buildOverlay();
        state.urls    = [src];
        state.idx     = 0;
        state.type    = 'video';
        state.postCard = postCard || null;

        imgEl.style.display  = 'none';
        videoEl.style.display = 'block';
        videoEl.poster = poster || '';

        // Attach hls.js or native
        _attachViewerVideo(src);

        prevBtn.style.display = 'none';
        nextBtn.style.display = 'none';
        counterEl.textContent = '';
        _renderActions();
        overlay.style.display = 'flex';
        document.body.style.overflow = 'hidden';
        videoEl.play().catch(function () {});
    }

    function _attachViewerVideo(src) {
        // Tear down any existing hls instance
        if (state.hlsInst) { try { state.hlsInst.destroy(); } catch(_) {} state.hlsInst = null; }
        videoEl.removeAttribute('src');
        videoEl.load();

        var canNative = videoEl.canPlayType('application/vnd.apple.mpegurl') !== '';
        if (canNative) {
            videoEl.src = src;
            return;
        }
        // Use global Hls if already loaded by video-player.js, otherwise load it
        var doAttach = function (Hls) {
            if (!Hls || !Hls.isSupported()) { videoEl.src = src; return; }
            var hls = new Hls({ maxBufferLength: 30, maxMaxBufferLength: 60, autoStartLoad: true });
            hls.loadSource(src);
            hls.attachMedia(videoEl);
            state.hlsInst = hls;
        };
        if (window.Hls) {
            doAttach(window.Hls);
        } else {
            var s = document.createElement('script');
            s.src = '/static/js/hls.min.js';
            s.onload = function () { doAttach(window.Hls); };
            document.head.appendChild(s);
        }
    }

    function _showImage(idx) {
        var url = state.urls[idx];
        imgEl.src = url;
        imgEl.style.display = 'block';
        videoEl.style.display = 'none';

        var total = state.urls.length;
        counterEl.textContent = total > 1 ? (idx + 1) + ' / ' + total : '';

        prevBtn.style.display = (total > 1 && idx > 0)           ? 'flex' : 'none';
        nextBtn.style.display = (total > 1 && idx < total - 1)   ? 'flex' : 'none';
    }

    function navigate(delta) {
        if (state.type !== 'image') return;
        var next = state.idx + delta;
        if (next < 0 || next >= state.urls.length) return;
        state.idx = next;
        _showImage(state.idx);
    }

    // ── Action bar ────────────────────────────────────────────────────────────────
    function _renderActions() {
        actionsEl.innerHTML = '';
        if (!state.postCard) return;

        // Collect like / reply / bookmark buttons from the originating card
        var likeBtn = state.postCard.querySelector('[hx-post="/feed/item/like"]');
        var replyLink = state.postCard.querySelector('[href^="/post/"]');
        var bookBtn = state.postCard.querySelector('[hx-post="/feed/item/bookmark"]');

        if (likeBtn) {
            var lb = _actionClone(likeBtn, 'Like');
            actionsEl.appendChild(lb);
        }
        if (replyLink) {
            var href = replyLink.getAttribute('href');
            var rb = _actionLink(href, replyIcon(), 'Reply');
            actionsEl.appendChild(rb);
        }
        if (bookBtn) {
            var bb = _actionClone(bookBtn, 'Save');
            actionsEl.appendChild(bb);
        }

        // Download (images only)
        if (state.type === 'image') {
            var dl = _actionLink(state.urls[state.idx], downloadIcon(), 'Download');
            dl.setAttribute('download', '');
            dl.setAttribute('target', '_blank');
            actionsEl.appendChild(dl);
        }
    }

    function _actionClone(original, label) {
        var wrap = document.createElement('button');
        wrap.style.cssText = 'display:flex;flex-direction:column;align-items:center;gap:3px;background:none;border:none;color:rgba(255,255,255,.8);cursor:pointer;font-size:11px;font-family:"DM Sans",sans-serif;min-width:44px';
        var clone = original.cloneNode(true);
        clone.style.cssText = 'width:34px;height:34px;border-radius:50%;background:rgba(255,255,255,.12);border:none;color:#fff;cursor:pointer;display:flex;align-items:center;justify-content:center';
        clone.removeAttribute('hx-post');
        clone.addEventListener('click', function (e) {
            e.stopPropagation();
            original.click();
        });
        wrap.appendChild(clone);
        var lbl = document.createElement('span');
        lbl.textContent = label;
        wrap.appendChild(lbl);
        return wrap;
    }

    function _actionLink(href, icon, label) {
        var a = document.createElement('a');
        a.href = href;
        a.style.cssText = 'display:flex;flex-direction:column;align-items:center;gap:3px;color:rgba(255,255,255,.8);text-decoration:none;font-size:11px;font-family:"DM Sans",sans-serif;min-width:44px';
        var iconWrap = document.createElement('span');
        iconWrap.style.cssText = 'width:34px;height:34px;border-radius:50%;background:rgba(255,255,255,.12);display:flex;align-items:center;justify-content:center;color:#fff';
        iconWrap.innerHTML = icon;
        a.appendChild(iconWrap);
        var lbl = document.createElement('span');
        lbl.textContent = label;
        a.appendChild(lbl);
        return a;
    }

    function replyIcon() {
        return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="width:16px;height:16px"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>';
    }
    function downloadIcon() {
        return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="width:16px;height:16px"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M7 10l5 5 5-5M12 15V3"/></svg>';
    }

    // ── Close ─────────────────────────────────────────────────────────────────────
    function close() {
        if (!overlay) return;
        overlay.style.display = 'none';
        document.body.style.overflow = '';
        // Pause + clean up the modal video
        try { videoEl.pause(); videoEl.src = ''; videoEl.load(); } catch (_) {}
        if (state.hlsInst) { try { state.hlsInst.destroy(); } catch(_) {} state.hlsInst = null; }
        imgEl.src = '';
    }

    // ── Event wiring ──────────────────────────────────────────────────────────────
    function wireClicks(root) {
        root = root || document;

        // Single image
        root.querySelectorAll('.post-media-img:not(.carousel-slide)').forEach(function (img) {
            if (img.dataset.fvWired) return;
            img.dataset.fvWired = '1';
            img.style.cursor = 'zoom-in';
            img.addEventListener('click', function (e) {
                e.stopPropagation();
                var card = img.closest('[data-content-id]') || img.closest('.post-card');
                openImages([img.src], 0, card);
            });
        });

        // Carousel images
        root.querySelectorAll('.media-carousel').forEach(function (carousel) {
            if (carousel.dataset.fvWired) return;
            carousel.dataset.fvWired = '1';
            carousel.querySelectorAll('.carousel-slide').forEach(function (slide, i) {
                slide.style.cursor = 'zoom-in';
                slide.addEventListener('click', function (e) {
                    e.stopPropagation();
                    var slides = Array.from(carousel.querySelectorAll('.carousel-slide'));
                    var urls = slides.map(function (s) { return s.src; });
                    var card = carousel.closest('[data-content-id]') || carousel.closest('.post-card');
                    openImages(urls, i, card);
                });
            });
        });

        // Video — clicking the poster/video opens the full-screen player
        root.querySelectorAll('video[data-f33d-hls]').forEach(function (vid) {
            if (vid.dataset.fvWired) return;
            vid.dataset.fvWired = '1';
            // Only open viewer on poster-area click (before play starts)
            vid.addEventListener('click', function (e) {
                // If already playing in-feed, don't hijack
                if (!vid.paused) return;
                e.stopPropagation();
                var card = vid.closest('[data-content-id]') || vid.closest('.post-card');
                openVideo(vid.dataset.f33dHls, vid.poster || '', card);
            });
        });
    }

    function init() {
        wireClicks(document);
        document.body.addEventListener('htmx:afterSwap', function (e) {
            wireClicks(e.detail && e.detail.target ? e.detail.target : document);
        });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }

    window.F33D3R_MediaViewer = { open: openImages, openVideo: openVideo, close: close };
})();
