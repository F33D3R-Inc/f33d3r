// f33d3r.js — Application logic (external, no template tags, no inline execution)
// All user data is read from <meta> or data- attributes on <body>.
// No JSON polling. Real-time via SSE (/api/events). No fetch() for data queries.

'use strict';

// ── Theme: read accent from body data attribute ───────────────────────────────
(function() {
  var app = document.getElementById('f33d3r-app');
  if (!app) return;
  var accent = app.dataset.accent;
  if (accent && /^#[0-9a-fA-F]{6}$/.test(accent)) {
    app.style.setProperty('--accent', accent);
    app.style.setProperty('--accent-muted', accent + 'AA');
  }
})();

// ── Shell runtime: Playground navigation ─────────────────────────────────────
// This document is the Shell. It is created once per session and never loaded
// again: every navigation asks the server for the next page's Playground and
// htmx swaps it into #playground (body carries hx-boost). This block owns the
// browser's side of that contract, and nothing more:
//   1. a boosted request is retargeted onto #playground (hx-target on the body
//      would be inherited by every hx-get on the site, so it lives here),
//   2. the page-script registry: a page file calls F33D3R.page(name, mount) and
//      is executed once per session; when its page arrives again the mount is
//      re-run against the fresh Playground instead of the script re-executing,
//   3. teardown of the page that is leaving (a mount may return a teardown),
//   4. F33D3R.visit / F33D3R.refresh for the few places script learns where to
//      go next from a server answer,
//   5. anchors htmx has not processed (markup FA Live inserted without
//      htmx.process) navigate the Playground too.
window.F33D3R = window.F33D3R || {};
(function () {
  var F = window.F33D3R;
  var SWAP = 'innerHTML show:window:top';
  var registry = {};   // page name → mount(root) → optional teardown()
  var active   = [];   // mounts live in the current Playground
  var loaded   = {};   // script path (no query) → executed in this document
  var pending  = [];   // page names whose script was stripped from an incoming Playground
  var onced    = {};   // keys claimed through F33D3R.once

  function playground() { return document.getElementById('playground'); }
  function pathOf(src) {
    try { return new URL(src, location.href).pathname; } catch (err) { return String(src).split('?')[0]; }
  }
  function nameOf(path) { return path.replace(/^.*\//, '').replace(/\.js$/, ''); }
  function seed() {
    document.querySelectorAll('script[src]').forEach(function (el) {
      loaded[pathOf(el.getAttribute('src'))] = true;
    });
  }
  seed();
  document.addEventListener('DOMContentLoaded', seed);

  function run(name) {
    var mount = registry[name];
    if (!mount) return;
    var teardown = null;
    try { teardown = mount(playground() || document); }
    catch (err) { console.error('[shell] mount failed: ' + name, err); }
    active.push({ name: name, teardown: (typeof teardown === 'function') ? teardown : null });
  }
  function teardownAll() {
    var leaving = active;
    active = [];
    leaving.forEach(function (a) {
      if (!a.teardown) return;
      try { a.teardown(); } catch (err) { console.error('[shell] teardown failed: ' + a.name, err); }
    });
  }
  // Page-scoped overlays do not outlive the page that opened them.
  function clearOverlays() {
    ['overlay-slot', 'work-menu-overlay'].forEach(function (id) {
      var el = document.getElementById(id);
      if (el) el.innerHTML = '';
    });
  }

  // F33D3R.page — register a page file's mount and run it now. `name` is the
  // script's file name without .js; that is how an incoming Playground's
  // <script src> is matched back to its mount.
  F.page = function (name, mount) {
    registry[name] = mount;
    var cs = document.currentScript;
    if (cs && cs.getAttribute('src')) loaded[pathOf(cs.getAttribute('src'))] = true;
    run(name);
  };
  // F33D3R.once — run fn once per document, whatever page mounts it. For the
  // document-level listeners a page file installs.
  F.once = function (key, fn) {
    if (onced[key]) return;
    onced[key] = true;
    fn();
  };

  var SCRIPT_RE = /<script\b([^>]*)\bsrc=["']([^"']+)["']([^>]*)>\s*<\/script>/gi;
  // A script src the document already executed is removed from the incoming
  // Playground and its mount is queued instead; one not yet seen stays in so
  // htmx executes it once — it registers itself through F33D3R.page.
  function dedupScripts(html) {
    pending = [];
    return html.replace(SCRIPT_RE, function (tag, before, src) {
      var path = pathOf(src);
      if (loaded[path]) { pending.push(nameOf(path)); return ''; }
      loaded[path] = true;
      return tag;
    });
  }

  function isPlaygroundResponse(d) {
    var pg = playground();
    if (!pg) return false;
    if (d.boosted) return true;
    if (d.target && d.target.id === 'playground') return true;
    return !!(d.requestConfig && d.requestConfig.target && d.requestConfig.target.id === 'playground');
  }

  // 1 + 2 + 3: a Playground answer is retargeted, deduplicated and the leaving
  // page torn down before the swap.
  document.addEventListener('htmx:beforeSwap', function (e) {
    var d = e.detail;
    if (!d || !isPlaygroundResponse(d)) return;
    var html = d.serverResponse || '';
    if (d.xhr && d.xhr.getResponseHeader('HX-Redirect')) return; // htmx loads the document
    // A whole document is not a Playground: the server drew its own Shell for
    // this URL (an auth page, a handler outside render()). Load it as one.
    if (/^\s*(<!doctype|<html)/i.test(html)) {
      d.shouldSwap = false;
      var doc = (d.xhr && d.xhr.responseURL) || (d.pathInfo && d.pathInfo.finalRequestPath);
      if (doc) location.href = doc;
      return;
    }
    d.target = playground();
    d.swapOverride = SWAP;
    // The page the server drew for an error status is still the page for this
    // navigation (a 404 is a page), so it is shown rather than dropped.
    if (html) d.shouldSwap = true;
    d.serverResponse = dedupScripts(html);
    teardownAll();
    clearOverlays();
  });

  // Back/forward: htmx asks the server for the Playground again and swaps
  // #playground (hx-history-elt); the same dedup and teardown apply.
  document.addEventListener('htmx:historyCacheMissLoad', function (e) {
    var d = e.detail;
    if (!d || typeof d.response !== 'string') return;
    var redirect = d.xhr && d.xhr.getResponseHeader('HX-Redirect');
    if (redirect) { d.response = ''; location.href = redirect; return; }
    d.response = dedupScripts(d.response);
    teardownAll();
    clearOverlays();
  });

  // After the Playground is in place, the mounts whose scripts were already
  // loaded run against it. Scripts that were left in execute on their own.
  document.addEventListener('htmx:afterSwap', function (e) {
    if (e.target !== playground()) return;
    var names = pending;
    pending = [];
    names.forEach(run);
  });

  // 4: programmatic Playground navigation. visit pushes history; refresh asks
  // for the current page again and replaces the entry.
  F.visit = function (url) {
    if (!window.htmx || !playground()) { location.href = url; return; }
    htmx.ajax('GET', url, { target: '#playground', swap: SWAP, push: 'true' });
  };
  F.refresh = function () {
    if (!window.htmx || !playground()) { location.reload(); return; }
    htmx.ajax('GET', location.pathname + location.search, { target: '#playground', swap: SWAP, replace: 'true' });
  };

  // 5: an anchor htmx never processed (a card FA Live inserted without
  // htmx.process) still navigates the Playground. htmx's own listener sits on
  // the element and runs first; a boosted anchor is left to it.
  document.addEventListener('click', function (e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    if (!a || !playground() || !window.htmx) return;
    var internal = a['htmx-internal-data'];
    if (internal && internal.boosted) return;
    if (a.closest('[hx-boost="false"], [hx-disable], [data-hx-disable]')) return;
    if (a.hasAttribute('download') || a.hasAttribute('hx-get') || a.hasAttribute('hx-post')) return;
    var target = a.getAttribute('target');
    if (target && target !== '_self') return;
    var href = a.getAttribute('href') || '';
    if (!href || href.charAt(0) === '#' || /^(javascript|mailto|tel|sms):/i.test(href)) return;
    var u;
    try { u = new URL(a.href, location.href); } catch (err) { return; }
    if (u.origin !== location.origin) return;
    e.preventDefault();
    F.visit(u.pathname + u.search + u.hash);
  });
})();

// ── SSE: connect to /api/events for real-time notifications ───────────────────
// The Go server pushes HTML fragments. HTMX swaps them into the DOM.
// No polling. No fetch(). No JSON.
(function() {
  var pialMeta = document.querySelector('meta[name="f33d3r:pial"]');
  if (!pialMeta) return; // not logged in

  // Single owner of /api/events for the whole app. A duplicate inline EventSource used to
  // live in base.html; two connections meant every event (notify/balance/…) could be delivered
  // and appended twice. This is now the only subscriber. Reconnects on drop with capped
  // exponential backoff.
  var reconnectDelay = 1000;
  var reconnectTimer = null;

  // bind attaches every FA Live listener to a connection. Called on first connect and on
  // each reconnect (a fresh EventSource needs its listeners re-bound).
  function bind(es) {
    es.onopen = function() { reconnectDelay = 1000; };

    es.addEventListener('notify', function(e) {
      // Update badge counter in sidebar/nav
      document.querySelectorAll('.sse-notif-target').forEach(function(el) {
        el.innerHTML = e.data;
      });
      window._onSSENotify && window._onSSENotify(e);
    });

    // notif_new fires when unread count increases — triggers notification list refresh
    // on the /notifications page without a full reload.
    es.addEventListener('notif_new', function(e) {
      window._onSSENotifNew && window._onSSENotifNew(e);
      var list = document.getElementById('notification-list');
      if (list && typeof htmx !== 'undefined') {
        htmx.trigger(list, 'refresh');
      }
    });

    es.addEventListener('balance', function(e) {
      if (!e.data) return;
      var el = document.getElementById('sidebar-aet-bal');
      if (el) el.textContent = e.data;
      var r = document.getElementById('right-aet-bal');
      if (r) r.textContent = e.data;
      // Wallet page — keeps the main balance display live without polling
      var px = document.querySelector('.pixel-balance');
      if (px) px.textContent = e.data.replace(' AET', '');
      var m = document.getElementById('mobile-aet-bal');
      if (m) m.textContent = e.data;
    });

    es.addEventListener('achievement.unlocked', function(e) {
      if (!e.data) return;
      try { showAchievementToast(JSON.parse(e.data)); } catch (_) {}
    });

    es.addEventListener('follow_state', function(e) {
      window._onSSEFollowState && window._onSSEFollowState(e);
    });

    // new_post: server sends fully rendered post_card HTML.
    // Buffer it client-side; show banner with count. Never parse as JSON.
    es.addEventListener('new_post', function(e) {
      if (!e.data) return;
      // Only show the banner on the home feed page
      var feedContainer = document.getElementById('feed-container');
      if (!feedContainer) return;
      window._pendingPosts = window._pendingPosts || [];
      window._pendingPosts.push(e.data);
      var count = window._pendingPosts.length;
      var countEl = document.getElementById('new-post-count');
      if (countEl) {
        countEl.textContent = count + ' new post' + (count === 1 ? '' : 's');
      }
      var banner = document.getElementById('new-post-banner');
      if (banner) banner.style.display = 'flex';
    });

    es.addEventListener('post_deleted', function(e) {
      // Data is the raw work UUID. Locate all matching article elements in the DOM and remove them.
      var workID = (e.data || '').trim();
      if (!workID) return;
      document.querySelectorAll('article[data-facet-id="work:' + workID + '"]').forEach(function(el) {
        el.style.transition = 'opacity 0.25s';
        el.style.opacity = '0';
        setTimeout(function() { el.parentNode && el.parentNode.removeChild(el); }, 260);
      });
    });

    // ── Listeners migrated from the former base.html inline connection ──────────────
    es.addEventListener('post_engagement', function(e) { window._onSSEPostEngagement && window._onSSEPostEngagement(e); });
    es.addEventListener('price_update',    function(e) { window._onSSEPriceUpdate    && window._onSSEPriceUpdate(e);    });
    es.addEventListener('tip_received',    function(e) { window._onSSETipReceived    && window._onSSETipReceived(e);    });
    es.addEventListener('article_view',    function(e) { window._onSSEArticleView    && window._onSSEArticleView(e);    });
    es.addEventListener('analytics_tick',  function(e) { window._onSSEAnalyticsTick  && window._onSSEAnalyticsTick(e);  });
    es.addEventListener('new_reply',       function(e) { window._onSSENewReply       && window._onSSENewReply(e);       });
    // Live lane: one rendered chat row, and any re-rendered live Facet
    // (viewer tally, LIVE badge, player, feed card). live-chat.js applies them.
    es.addEventListener('live_chat',       function(e) { window._onSSELiveChat       && window._onSSELiveChat(e);       });
    es.addEventListener('live_facet',      function(e) { window._onSSELiveFacet      && window._onSSELiveFacet(e);      });
    es.addEventListener('frequency_facet', function(e) { window._onSSEFrequencyFacet && window._onSSEFrequencyFacet(e); });
    // Sports lane: a re-rendered game Facet (strip, card, score, clock, rail).
    es.addEventListener('sports_facet',    function(e) { window._onSSESportsFacet    && window._onSSESportsFacet(e);    });
    // Gnosis: a new message bubble arrived. Append it live if its thread is open,
    // otherwise bump the Messages badge and refresh the conversation list (if shown).
    es.addEventListener('gnosis_message', function(e) {
      if (!e.data) return;
      var tmp = document.createElement('div');
      tmp.innerHTML = e.data.trim();
      var bubble = tmp.firstElementChild;
      if (!bubble) return;
      var convoId = bubble.getAttribute('data-convo');
      var thread = document.querySelector('.gn-thread[data-convo-id="' + convoId + '"]');
      var list = document.getElementById('gnosis-messages');
      if (thread && list) {
        list.appendChild(bubble);
        list.scrollTop = list.scrollHeight;
        if (window._onGnosisBubble) window._onGnosisBubble(bubble); // sealed-mode opener hook
      } else {
        var badge = document.getElementById('dm-badge');
        if (badge) {
          var n = (parseInt(badge.dataset.count || '0', 10) || 0) + 1;
          badge.dataset.count = n;
          badge.textContent = n > 9 ? '9+' : n;
          badge.style.display = '';
        }
        if (document.getElementById('gnosis-convos') && window.htmx) {
          window.htmx.trigger(document.body, 'gnosis-refresh');
        }
      }
    });
    es.addEventListener('leaderboard_refresh', function(e) {
      window._onSSELeaderboardRefresh && window._onSSELeaderboardRefresh(e);
      // Refresh XP bar in sidebar live without page reload
      var bar = document.getElementById('xp-bar-wrap');
      if (bar && typeof htmx !== 'undefined') htmx.ajax('GET', '/facets/xp_bar', {target: '#xp-bar-wrap', swap: 'outerHTML'});
    });

    es.onerror = function() {
      // Reconnect with capped backoff (native retry is a fallback; this also resubscribes
      // cleanly after a server-side close such as the per-PIAL connection cap). onerror can
      // fire repeatedly during an outage — scheduleReconnect coalesces to ONE in-flight timer,
      // so we never spawn parallel EventSources (a second session double-delivers every event).
      es.close();
      scheduleReconnect();
    };
  }

  // One in-flight reconnect only: repeated onerror calls collapse into a single pending timer.
  function scheduleReconnect() {
    if (reconnectTimer) return;
    var d = reconnectDelay;
    reconnectDelay = Math.min(reconnectDelay * 2, 30000);
    reconnectTimer = setTimeout(function() { reconnectTimer = null; connect(); }, d);
  }

  function connect() {
    // Single owner of /api/events: tear down any prior connection (and pending reconnect timer)
    // before opening a new one, so a reconnect can never leave two registered SSE sessions. The
    // server fans every event to ALL of a PIAL's sessions, so a stray second one double-delivers.
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
    if (window.__F33D3R_SSE__) { try { window.__F33D3R_SSE__.close(); } catch (e) {} }
    var es = new EventSource('/api/events');
    // Expose globally so video-player.js can close on pagehide (memory hygiene).
    window.__F33D3R_SSE__ = es;
    bind(es);
  }

  connect();
})();

// ── showNewPosts: flush buffered SSE post cards into #feed-container ──────────
// Called by onclick="showNewPosts()" on #new-post-banner.
window.showNewPosts = function() {
  var pending = window._pendingPosts || [];
  if (!pending.length) return;
  var feedContainer = document.getElementById('feed-container');
  if (!feedContainer) return;

  // Prepend newest-first (array is oldest-first from SSE, so reverse before inserting)
  var reversed = pending.slice().reverse();
  reversed.forEach(function(html) {
    var tmp = document.createElement('div');
    tmp.innerHTML = html;
    var article = tmp.firstElementChild;
    if (!article) return;
    feedContainer.insertBefore(article, feedContainer.firstChild);
    if (typeof htmx !== 'undefined') htmx.process(article);
  });

  // Reset buffer and banner
  window._pendingPosts = [];
  var banner = document.getElementById('new-post-banner');
  if (banner) banner.style.display = 'none';
  var countEl = document.getElementById('new-post-count');
  if (countEl) countEl.textContent = '0';

  // Format timestamps on the newly prepended posts
  if (typeof formatPostTimes === 'function') formatPostTimes(feedContainer);

  // Scroll to top so the new posts are visible
  window.scrollTo({ top: 0, behavior: 'smooth' });
};

// ── FA Live Layer 2: SSE handlers for live mutation events ───────────────────
// All event data is pre-rendered HTML — swap directly, never parse JSON.

window._onSSEPostEngagement = function(e) {
  if (!e.data) return;
  var div = document.createElement('div');
  div.innerHTML = e.data;
  var el = div.firstElementChild;
  if (el && el.id) {
    var target = document.getElementById(el.id);
    if (target) target.outerHTML = e.data;
  }
};

// sports_facet: the server sends a complete, already-rendered sports Facet.
// The browser's whole job is to find the element that claims that facet id and
// swap it for the fragment. It parses no score, derives no clock and decides
// nothing about the game — if the page does not mount that facet, nothing
// happens. Rendering stays on the server; this is a projection surface.
window._onSSESportsFacet = function(e) {
  if (!e.data) return;
  var tmp = document.createElement('div');
  tmp.innerHTML = e.data.trim();
  var incoming = tmp.firstElementChild;
  if (!incoming) return;
  var facetID = incoming.getAttribute('data-facet-id');
  if (!facetID) return;
  var target = document.querySelector('[data-facet-id="' + facetID + '"]');
  if (!target) return;
  target.replaceWith(incoming);
};

window._onSSEPriceUpdate = function(e) {
  if (!e.data) return;
  var div = document.createElement('div');
  div.innerHTML = e.data;
  var el = div.firstElementChild;
  if (!el) return;
  var ticker = el.dataset && el.dataset.ticker;
  if (ticker) {
    var existing = document.querySelector('[data-ticker="' + ticker + '"]');
    if (existing) existing.outerHTML = e.data;
  }
};

window._onSSETipReceived = function(e) {
  if (!e.data) return;
  var container = document.getElementById('toast-container');
  if (container) {
    var wrapper = document.createElement('div');
    wrapper.innerHTML = e.data;
    container.appendChild(wrapper.firstElementChild || wrapper);
  }
};

// follow_state: the server sends a finished btn_follow fragment, one per surface.
// This locates the matching facets and swaps the fragment in. It builds nothing:
// the class, the label and the next hx-vals payload all arrive already rendered.
window._onSSEFollowState = function(e) {
  if (!e.data) return;
  var wrap = document.createElement('div');
  wrap.innerHTML = e.data.trim();
  var neo = wrap.firstElementChild;
  if (!neo) return;
  var facetID = neo.getAttribute('data-facet-id');
  var surface = neo.getAttribute('data-surface');
  if (!facetID || !surface) return;
  // Same facet, same surface: a viewer can have this person on screen in more
  // than one shape at once, and each shape has its own fragment on the stream.
  var sel = '[data-facet-id="' + facetID + '"][data-surface="' + surface + '"]';
  document.querySelectorAll(sel).forEach(function(old) {
    var frag = neo.cloneNode(true);
    old.replaceWith(frag);
    if (typeof htmx !== 'undefined') htmx.process(frag);
  });
};

window._onSSEArticleView = function(e) {
  if (!e.data) return;
  var div = document.createElement('div');
  div.innerHTML = e.data;
  var el = div.firstElementChild;
  if (el && el.id) {
    var target = document.getElementById(el.id);
    if (target) target.outerHTML = e.data;
  }
};

window._onSSEAnalyticsTick = function(e) {
  if (!e.data) return;
  var div = document.createElement('div');
  div.innerHTML = e.data;
  var el = div.firstElementChild;
  if (el && el.id) {
    var target = document.getElementById(el.id);
    if (target) target.outerHTML = e.data;
  }
};

// ── FA Live Layer 2: IntersectionObserver — viewport watch signals ────────────
// Fires POST /api/events/watch when post cards or cashtag cards enter/exit viewport.
// Re-initialises on every HTMX swap to pick up newly injected cards.
(function() {
  if (typeof IntersectionObserver === 'undefined') return;

  function _watch(type, id, action) {
    var body = 'type=' + encodeURIComponent(type) +
               '&id=' + encodeURIComponent(id) +
               '&action=' + encodeURIComponent(action);
    fetch('/api/events/watch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: body,
      keepalive: true
    });
  }

  var postObs = new IntersectionObserver(function(entries) {
    entries.forEach(function(entry) {
      var id = entry.target.dataset.contentId;
      if (id) _watch('post', id, entry.isIntersecting ? 'watch' : 'unwatch');
    });
  }, { threshold: 0.2 });

  var tickerObs = new IntersectionObserver(function(entries) {
    entries.forEach(function(entry) {
      var ticker = entry.target.dataset.ticker;
      if (ticker) _watch('ticker', ticker, entry.isIntersecting ? 'watch' : 'unwatch');
    });
  }, { threshold: 0.2 });

  function _observeAll(root) {
    var ctx = root || document;
    ctx.querySelectorAll('article.post-card[data-content-id]').forEach(function(el) {
      postObs.observe(el);
    });
    ctx.querySelectorAll('[data-ticker]').forEach(function(el) {
      tickerObs.observe(el);
    });
  }

  document.addEventListener('DOMContentLoaded', function() { _observeAll(document); });
  document.addEventListener('htmx:afterSwap', function(e) {
    if (e.detail && e.detail.elt) _observeAll(e.detail.elt);
  });
})();

// ── Compose: close and reset on any HTMX page navigation ─────────────────────
// The compose modal lives in #overlay-slot which is NOT a nav target, so it
// survives HTMX page swaps with any typed text intact. Reset it on every nav.
document.addEventListener('htmx:pushedIntoHistory', function() {
  var modal = document.getElementById('compose-modal');
  if (modal && !modal.classList.contains('hidden')) {
    if (window.closeCompose) { window.closeCompose(); }
  }
});

// ── Achievement toast ─────────────────────────────────────────────────────────
function showAchievementToast(data) {
  var existing = document.getElementById('achievement-toast');
  if (existing) existing.remove();

  var toast = document.createElement('div');
  toast.id = 'achievement-toast';
  toast.className = 'achievement-toast' + (data.rarity ? ' achievement-toast--' + data.rarity : '');
  toast.innerHTML =
    '<div class="achievement-toast-icon">' + (data.icon || '🏆') + '</div>' +
    '<div class="achievement-toast-body">' +
      '<div class="achievement-toast-label">Achievement Unlocked</div>' +
      '<div class="achievement-toast-name">' + (data.name || '') + '</div>' +
      '<div class="achievement-toast-xp">+' + (data.xp_reward || 0) + ' XP</div>' +
    '</div>';

  document.body.appendChild(toast);
  requestAnimationFrame(function() {
    requestAnimationFrame(function() { toast.classList.add('visible'); });
  });

  var delay = ['rare','epic','legendary','mythic'].indexOf(data.rarity) !== -1 ? 10000 : 5000;
  setTimeout(function() {
    toast.classList.remove('visible');
    setTimeout(function() { if (toast.parentNode) toast.remove(); }, 400);
  }, delay);
}

// ── Edit profile modal ────────────────────────────────────────────────────────
window.closeEditProfile = function() {
  var slot = document.getElementById('overlay-slot');
  if (slot) slot.innerHTML = '';
};

// ── Add-account modal ─────────────────────────────────────────────────────────
window.closeAddAccountModal = function() {
  var slot = document.getElementById('overlay-slot');
  if (slot) slot.innerHTML = '';
};

// ── Compose modal ─────────────────────────────────────────────────────────────
window.closeCompose = function() {
  document.getElementById('compose-modal').classList.add('hidden');
  // Reset poll mode so the next compose session starts clean
  if (_pollMode) {
    _pollMode = false;
    var _pp = document.getElementById('poll-compose-panel');
    if (_pp) _pp.style.display = 'none';
    var _pb = document.getElementById('compose-poll-btn');
    if (_pb) _pb.classList.remove('active');
  }
  _clearThreadSegments();
  // Reset reply state
  var modal = document.getElementById('compose-modal');
  if (modal) {
    var pi = modal.querySelector('input[name="parent_id"]');
    if (pi) pi.remove();
    var hdr = modal.querySelector('.compose-header span');
    if (hdr) hdr.textContent = 'New post';
  }
  // Reset compose preview slot and link preview state
  var slot = document.getElementById('compose-preview-slot');
  if (slot) slot.innerHTML = '';
  var dismissField = document.getElementById('compose-dismiss-link-preview');
  if (dismissField) dismissField.value = '';
  _composeLinkPreviewDismissed = false;
  _composeLinkPreviewURL = '';
  clearTimeout(_composeLinkPreviewTimer);
  // Reset cashtag state
  _closeCashtagDropdown();
  var ctHidden = document.getElementById('compose-cashtag-ticker');
  if (ctHidden) ctHidden.value = '';
  // Reset hashtag autocomplete + clear the server-rendered highlight backdrop.
  if (typeof _closeHashtagDropdown === 'function') _closeHashtagDropdown();
  var hl = document.getElementById('compose-highlighter');
  if (hl) hl.innerHTML = '';
};

// ── Compose preview slot — link preview detection ─────────────────────────────
// LISTEN/LOCATE/PATCH/EMIT pattern for live link previews while composing.
var _composeLinkPreviewTimer = null;
var _composeLinkPreviewURL   = '';
var _composeLinkPreviewDismissed = false;

function _composeExtractURL(text) {
  // Match full URLs (with protocol) first; fall back to bare domains like apple.com
  var m = text.match(/https?:\/\/[^\s"'<>]+/);
  if (m) return m[0].replace(/[.,;:!?)'"\]]+$/, '');
  // Bare domain: word.tld or word.tld/path — must have a real TLD (2+ chars), not @mentions or file extensions
  var bare = text.match(/(?:^|\s)((?:www\.)?[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z]{2,})+(?:\/[^\s"'<>]*)?)/);
  if (!bare) return null;
  var url = bare[1].replace(/[.,;:!?)'"\]]+$/, '');
  // Reject short fragments that are likely not URLs (e.g. "e.g" or "i.e")
  if (url.length < 5 || /^[a-z]\.g$|^i\.e$|^e\.g$/i.test(url)) return null;
  return 'https://' + url;
}

function _composeFetchLinkPreview(rawURL) {
  var slot = document.getElementById('compose-preview-slot');
  if (!slot) return;
  // Don't clobber a quoted-post preview
  if (slot.querySelector('.cps-wrap--quote')) return;
  slot.innerHTML = '<div class="cps-loading">Fetching preview…</div>';
  fetch('/facets/link_preview?url=' + encodeURIComponent(rawURL))
    .then(function(r) {
      if (r.status === 204 || !r.ok) return null;
      return r.text();
    })
    .then(function(html) {
      if (!slot) return;
      if (!html) { slot.innerHTML = ''; return; }
      slot.innerHTML =
        '<div class="cps-wrap cps-wrap--link">' +
          '<button type="button" class="cps-dismiss" onclick="window.dismissComposeLinkPreview()" aria-label="Remove link preview">×</button>' +
          html +
        '</div>';
    })
    .catch(function() { if (slot) slot.innerHTML = ''; });
}

window.dismissComposeLinkPreview = function() {
  var slot = document.getElementById('compose-preview-slot');
  if (slot) slot.innerHTML = '';
  _composeLinkPreviewDismissed = true;
  _composeLinkPreviewURL = '';
  var field = document.getElementById('compose-dismiss-link-preview');
  if (field) field.value = '1';
};

// Wire URL detection to the compose textarea on DOMContentLoaded
document.addEventListener('DOMContentLoaded', function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  ta.addEventListener('input', function() {
    if (_composeLinkPreviewDismissed) return;
    var slot = document.getElementById('compose-preview-slot');
    // Don't overwrite a quote preview
    if (slot && (slot.querySelector('.cps-wrap--quote') || slot.querySelector('.cps-wrap--cashtag'))) return;
    var detected = _composeExtractURL(ta.value);
    if (!detected) {
      // URL removed from text — clear preview
      if (slot) slot.innerHTML = '';
      _composeLinkPreviewURL = '';
      return;
    }
    if (detected === _composeLinkPreviewURL) return; // same URL, no re-fetch
    _composeLinkPreviewURL = detected;
    clearTimeout(_composeLinkPreviewTimer);
    _composeLinkPreviewTimer = setTimeout(function() {
      _composeFetchLinkPreview(detected);
    }, 700);
  });
});
// ── Cashtag autocomplete — LISTEN/LOCATE/PATCH/EMIT ─────────────────────────
// Detects $TICKER pattern in the compose textarea, fetches matching tickers
// from /facets/cashtag/search, shows a dropdown, and on selection injects
// the stock card into the compose-preview-slot.

var _cashtagDropTimer = null;
var _cashtagActive = false;
var _cashtagCurrentQuery = '';

function _closeCashtagDropdown() {
  var dd = document.getElementById('cashtag-dropdown');
  if (dd) { dd.style.display = 'none'; dd.innerHTML = ''; }
  _cashtagActive = false;
  _cashtagCurrentQuery = '';
}

function _showCashtagDropdown(html) {
  var dd = document.getElementById('cashtag-dropdown');
  if (!dd) return;
  if (!html || html.trim() === '') { _closeCashtagDropdown(); return; }
  dd.innerHTML = html;
  var _ta = document.getElementById('compose-textarea');
  if (_ta) window.positionDropdownAtCaret(_ta, dd);
  dd.style.display = 'block';
  _cashtagActive = true;
}

// Called by onclick on each cashtag_autocomplete_row
window.onCashtagSelect = function(ticker, name) {
  _closeCashtagDropdown();
  // Insert $TICKER at cursor or end of textarea
  var ta = document.getElementById('compose-textarea');
  if (ta) {
    var pos = ta.selectionStart;
    var val = ta.value;
    // Remove the partial $query the user was typing
    var before = val.slice(0, pos).replace(/\$[A-Z]*$/, function() { return '$' + ticker; });
    var after  = val.slice(pos);
    ta.value = before + after;
    var newPos = before.length;
    ta.setSelectionRange(newPos, newPos);
    ta.focus();
    updateCharCount();
  }
  // Store ticker in hidden input
  var hidden = document.getElementById('compose-cashtag-ticker');
  if (hidden) hidden.value = ticker;
  // Fetch and inject the stock card into the preview slot
  var slot = document.getElementById('compose-preview-slot');
  if (slot) {
    slot.innerHTML = '<div class="cps-loading">Loading $' + ticker + '…</div>';
    fetch('/facets/cashtag/card?ticker=' + encodeURIComponent(ticker))
      .then(function(r) { return r.ok ? r.text() : null; })
      .then(function(html) {
        if (!slot) return;
        if (!html) { slot.innerHTML = ''; return; }
        slot.innerHTML = '<div class="cps-wrap cps-wrap--cashtag">' + html + '</div>';
      })
      .catch(function() { if (slot) slot.innerHTML = ''; });
  }
};

// Wire cashtag detection to compose textarea
document.addEventListener('DOMContentLoaded', function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  ta.addEventListener('input', function() {
    var val = ta.value;
    var pos = ta.selectionStart;
    var before = val.slice(0, pos);
    // Detect $ followed by 1-5 uppercase letters at the cursor
    var match = before.match(/\$([A-Z]{1,5})$/);
    if (!match) {
      _closeCashtagDropdown();
      return;
    }
    var q = match[1];
    if (q === _cashtagCurrentQuery) return;
    _cashtagCurrentQuery = q;
    clearTimeout(_cashtagDropTimer);
    _cashtagDropTimer = setTimeout(function() {
      fetch('/facets/cashtag/search?q=' + encodeURIComponent(q))
        .then(function(r) { return r.ok ? r.text() : ''; })
        .then(_showCashtagDropdown)
        .catch(function() { _closeCashtagDropdown(); });
    }, 300);
  });
  // Close dropdown on Escape or click outside
  document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape' && _cashtagActive) _closeCashtagDropdown();
  });
  document.addEventListener('click', function(e) {
    var dd = document.getElementById('cashtag-dropdown');
    if (dd && !dd.contains(e.target) && e.target !== ta) _closeCashtagDropdown();
  });
});

// ── Hashtag autocomplete (#tag) ───────────────────────────────────────────────
// Mirrors the cashtag dropdown. FA-pure: rows are server-rendered HTML from
// /facets/hashtag/search (most-used tags by prefix); the client only swaps them in
// and, on selection, edits the textarea. Anchored under the caret like the others.
var _hashtagDropTimer = null;
var _hashtagActive = false;
var _hashtagCurrentQuery = '';
function _closeHashtagDropdown() {
  var dd = document.getElementById('hashtag-dropdown');
  if (dd) { dd.style.display = 'none'; dd.innerHTML = ''; }
  _hashtagActive = false;
  _hashtagCurrentQuery = '';
}
function _showHashtagDropdown(html) {
  var dd = document.getElementById('hashtag-dropdown');
  if (!dd) return;
  if (!html || html.trim() === '') { _closeHashtagDropdown(); return; }
  dd.innerHTML = html;
  var ta = document.getElementById('compose-textarea');
  if (ta && window.positionDropdownAtCaret) window.positionDropdownAtCaret(ta, dd);
  dd.style.display = 'block';
  _hashtagActive = true;
}
window.onHashtagSelect = function(tag) {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  var pos = ta.selectionStart;
  var before = ta.value.slice(0, pos).replace(/#[A-Za-z0-9_]*$/, '#' + tag + ' ');
  var after = ta.value.slice(pos);
  ta.value = before + after;
  ta.selectionStart = ta.selectionEnd = before.length;
  ta.focus();
  _closeHashtagDropdown();
  // Programmatic edit: fire input so the highlight backdrop + char count refresh.
  ta.dispatchEvent(new Event('input', { bubbles: true }));
};

// ── Compose highlight backdrop (client-rendered) ──────────────────────────────
// Paints @mention / #hashtag / $CASHTAG tokens in the accent color on the
// backdrop behind the transparent-text textarea. Runs synchronously on every
// input event, so the colored text appears in the SAME frame as the keystroke —
// no server round-trip, therefore none of the blank/blinking the old HTMX-driven
// version caused. Token set mirrors the server linkifier (highlightComposeText).
window.renderComposeHighlight = function(ta) {
  ta = ta || document.getElementById('compose-textarea');
  var hl = document.getElementById('compose-highlighter');
  if (!ta || !hl) return;
  var text = ta.value;
  function esc(s) { return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
  var re = /@[A-Za-z0-9_]{1,50}|#[A-Za-z0-9_]{1,100}|\$[A-Z]{1,5}\b/g;
  var out = '', last = 0, m;
  while ((m = re.exec(text)) !== null) {
    out += esc(text.slice(last, m.index));
    out += '<span class="cmp-tok">' + esc(m[0]) + '</span>';
    last = m.index + m[0].length;
  }
  out += esc(text.slice(last));
  // A trailing newline collapses in HTML — add a filler so the backdrop's last
  // line keeps its height and stays aligned with the textarea caret.
  if (text.charAt(text.length - 1) === '\n') out += '&nbsp;';
  hl.innerHTML = out;
  hl.scrollTop = ta.scrollTop;
  hl.scrollLeft = ta.scrollLeft;
};

document.addEventListener('DOMContentLoaded', function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  ta.addEventListener('input', function() {
    var before = ta.value.slice(0, ta.selectionStart);
    var match = before.match(/#([A-Za-z0-9_]{1,100})$/);
    if (!match) { _closeHashtagDropdown(); return; }
    var q = match[1];
    if (q === _hashtagCurrentQuery && _hashtagActive) return;
    _hashtagCurrentQuery = q;
    clearTimeout(_hashtagDropTimer);
    _hashtagDropTimer = setTimeout(function() {
      fetch('/facets/hashtag/search?q=' + encodeURIComponent(q))
        .then(function(r) { return r.ok ? r.text() : ''; })
        .then(_showHashtagDropdown)
        .catch(function() { _closeHashtagDropdown(); });
    }, 200);
  });
  document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape' && _hashtagActive) _closeHashtagDropdown();
  });
  document.addEventListener('click', function(e) {
    var dd = document.getElementById('hashtag-dropdown');
    if (dd && !dd.contains(e.target) && e.target !== ta) _closeHashtagDropdown();
  });
  // Highlight backdrop: render tokens client-side on every keystroke (no blink),
  // and keep it scroll-aligned with the textarea as it scrolls.
  var hl = document.getElementById('compose-highlighter');
  ta.addEventListener('input', function() { window.renderComposeHighlight(ta); });
  if (hl) ta.addEventListener('scroll', function() { hl.scrollTop = ta.scrollTop; hl.scrollLeft = ta.scrollLeft; });
  window.renderComposeHighlight(ta);
});

// Facet(reply_compose) — opens compose modal pre-targeted at a work
window.openReplyCompose = function(postId, handle, name) {
  var modal = document.getElementById('compose-modal');
  if (!modal) return;

  // Find parent CID from the work card in the DOM.
  // work_card uses data-facet-id="work:ID"; work_detail_focus uses data-work-id="ID".
  var card = document.querySelector('[data-facet-id="work:' + postId + '"], [data-work-id="' + postId + '"]');
  var parentCID = card ? (card.dataset.workCid || '') : '';

  // Stash parent_id
  var pi = modal.querySelector('input[name="parent_id"]');
  if (!pi) { pi = document.createElement('input'); pi.type='hidden'; pi.name='parent_id'; modal.appendChild(pi); }
  pi.value = postId;

  // Stash parent_cid
  var pc = modal.querySelector('input[name="parent_cid"]');
  if (!pc) { pc = document.createElement('input'); pc.type='hidden'; pc.name='parent_cid'; modal.appendChild(pc); }
  pc.value = parentCID;

  // Update header label
  var hdr = modal.querySelector('.compose-header span');
  if (hdr) hdr.textContent = 'Reply to @' + handle;
  openCompose();
};

window.openCompose = function() {
  document.getElementById('compose-modal').classList.remove('hidden');
  // Always start fresh — text never persists between opens.
  // Explicit drafts (save button) are separate and unaffected.
  var ta = document.getElementById('compose-textarea');
  if (ta) { ta.value = ''; ta.style.height = 'auto'; }
  // Pre-fill with a profile mention if viewing someone else's page.
  var mainCol = document.getElementById('main-col');
  var mention = mainCol && mainCol.dataset.composeMention;
  if (mention && ta) {
    ta.value = mention + ' ';
    ta.style.height = 'auto';
    // Position cursor at end so user types after the mention.
    ta.setSelectionRange(ta.value.length, ta.value.length);
  }
  // Repaint the highlight backdrop for the (possibly pre-filled) value — a
  // programmatic value change does not fire the input event.
  if (ta) window.renderComposeHighlight(ta);
  setTimeout(function() {
    if (ta) ta.focus();
  }, 80);
};

// openComposeWithMention opens compose pre-filled with @handle for directed posts.
window.openComposeWithMention = function(mention) {
  window.openCompose();
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  ta.value = mention + ' ';
  ta.style.height = 'auto';
  ta.setSelectionRange(ta.value.length, ta.value.length);
  window.renderComposeHighlight(ta);
};

document.addEventListener('DOMContentLoaded', function() {
  // Navigation progress bar — uses #nav-progress from base.html
  var _npbar = (function(){
    var bar = document.getElementById('nav-progress');
    if (!bar) { bar = document.createElement('div'); document.body.appendChild(bar); }
    var t;
    return {
      start: function(){ clearTimeout(t); bar.style.opacity='1'; bar.style.width='72%'; },
      done:  function(){ bar.style.width='100%'; t=setTimeout(function(){ bar.style.opacity='0'; bar.style.width='0'; },320); }
    };
  })();
  document.body.addEventListener('htmx:beforeRequest', function(){ _npbar.start(); });
  document.body.addEventListener('htmx:afterSwap',     function(){ _npbar.done(); });
  document.body.addEventListener('htmx:responseError', function(){ _npbar.done(); });
  document.body.addEventListener('htmx:sendError',     function(){ _npbar.done(); });
  document.body.addEventListener('htmx:swapError',     function(){ _npbar.done(); });
  document.body.addEventListener('htmx:historyRestore', function(){ _npbar.done(); });
  // Show progress on regular anchor navigation too (instant visual feedback)
  document.addEventListener('click', function(e) {
    var a = e.target.closest('a[href]');
    if (!a || e.defaultPrevented || e.metaKey || e.ctrlKey || e.shiftKey) return;
    var href = a.getAttribute('href');
    if (!href || href.startsWith('#') || href.startsWith('javascript:') ||
        a.hasAttribute('hx-get') || a.hasAttribute('hx-post') || a.hasAttribute('download') ||
        a.getAttribute('target') === '_blank') return;
    _npbar.start();
  });

  // Compose open buttons
  var fab = document.getElementById('post-fab');
  var mobBtn = document.getElementById('mobile-post-btn');
  if (fab)    fab.addEventListener('click', openCompose);
  if (mobBtn) mobBtn.addEventListener('click', openCompose);

  // AER — Adaptive Facet Rendering: auto-detect compose mode
  ComposeAER.init();

  // Facet(nav_toolbar) — wrap nav items between nav-brand and nav-foot
  // into a scrollable toolbar layer so new links never overflow the sidebar.
  (function() {
    var sidebar = document.getElementById('sidebar');
    if (!sidebar) return;
    var brand = sidebar.querySelector('.nav-brand');
    var foot  = sidebar.querySelector('.nav-foot');
    if (!brand || !foot) return;
    var toolbar = document.createElement('div');
    toolbar.className = 'nav-toolbar';
    var node = brand.nextSibling;
    while (node && node !== foot) {
      var next = node.nextSibling;
      toolbar.appendChild(node);
      node = next;
    }
    sidebar.insertBefore(toolbar, foot);
  })();

  // Compose modal: backdrop click closes
  var overlay = document.getElementById('compose-modal');
  if (overlay) overlay.addEventListener('click', function(e) {
    if (e.target === overlay) closeCompose();
  });

  // Compose close button
  var closeBtn = document.getElementById('compose-close-btn');
  if (closeBtn) closeBtn.addEventListener('click', closeCompose);

  // Compose post button — dynamic lookup so poll/thread overrides are always used
  var postBtn = document.getElementById('compose-post-btn');
  if (postBtn) postBtn.addEventListener('click', function() { window.submitCompose(); });

  // Textarea input
  var ta = document.getElementById('compose-textarea');
  if (ta) ta.addEventListener('input', function() {
    onComposeInput(ta);
    updateCharCount();
  });

  // Media file input
  var mediaInput = document.getElementById('media-file-input');
  if (mediaInput) mediaInput.addEventListener('change', function() { onMediaSelect(mediaInput); });

  // Middle mouse wheel → horizontal scroll on compose toolbar
  var cActionBar = document.getElementById('compose-action-bar');
  if (cActionBar) {
    cActionBar.addEventListener('wheel', function(e) {
      if (Math.abs(e.deltaX) < Math.abs(e.deltaY)) {
        e.preventDefault();
        cActionBar.scrollLeft += e.deltaY;
      }
    }, { passive: false });
  }

  // Compose action buttons
  var pollBtn = document.getElementById('compose-poll-btn');
  if (pollBtn) pollBtn.addEventListener('click', togglePollCompose);

  var gifBtn = document.getElementById('compose-gif-btn');
  if (gifBtn) gifBtn.addEventListener('click', openGifPicker);

  var locBtn = document.getElementById('compose-location-btn');
  if (locBtn) locBtn.addEventListener('click', addComposeLocation);

  var mentionBtn = document.getElementById('compose-mention-btn');
  if (mentionBtn) mentionBtn.addEventListener('click', insertAtSymbol);

  var hashBtn = document.getElementById('compose-hashtag-btn');
  if (hashBtn) hashBtn.addEventListener('click', insertHashSymbol);

  var removeLocBtn = document.getElementById('compose-remove-location-btn');
  if (removeLocBtn) removeLocBtn.addEventListener('click', removeComposeLocation);

  // Thread compose
  var threadBtn = document.getElementById('compose-thread-btn');
  if (threadBtn) threadBtn.addEventListener('click', toggleThreadCompose);

  var threadAddBtn = document.getElementById('thread-add-btn');
  if (threadAddBtn) threadAddBtn.addEventListener('click', _addThreadSegment);

  // Poll add option button
  var pollAddBtn = document.getElementById('poll-add-btn');
  if (pollAddBtn) pollAddBtn.addEventListener('click', addPollOption);

  // NSFW checkbox label — sync active state visually
  var nsfwChk = document.getElementById('compose-nsfw');
  if (nsfwChk) nsfwChk.addEventListener('change', function() {
    var lbl = document.getElementById('compose-nsfw-label');
    if (lbl) lbl.classList.toggle('active', nsfwChk.checked);
  });

  // Mobile more overlay
  var mobOverlay = document.getElementById('mob-more-overlay');
  if (mobOverlay) mobOverlay.addEventListener('click', closeMobileMore);

  // Sidebar user menu
  var sidebarUserBtn = document.getElementById('sidebar-user-btn');
  if (sidebarUserBtn) sidebarUserBtn.addEventListener('click', toggleUserMenu);

  // Mobile more button
  var mobMoreBtn = document.getElementById('mob-more-btn');
  if (mobMoreBtn) mobMoreBtn.addEventListener('click', toggleMobileMore);

  // Malkuth crypto substrate init — async, non-blocking.
  if (window.Malkuth) {
    var _malkuthPIAL = (document.querySelector('meta[name="f33d3r:pial"]') || {}).content || '';
    if (_malkuthPIAL) {
      window._malkuthReady = window.Malkuth.init(_malkuthPIAL).catch(function(e) {
        console.warn('[malkuth] init failed:', e);
        return { hasKey: false };
      });
    }
  }
});

// ── Post-creation feed update helpers ────────────────────────────────────────
// Called after ANY successful post (regular, poll, thread) to update the UI
// without requiring a browser refresh.

// _insertPostCard: parses article HTML and prepends it to the first feed
// container found on the current page. Returns true if inserted.
function _insertPostCard(html) {
  if (!html || !html.trim()) return false;
  var tmp = document.createElement('div');
  tmp.innerHTML = html.trim();
  var article = tmp.querySelector('article');
  if (!article) return false;
  var feedIds = ['feed-container', 'profile-feed', 'explore-feed'];
  for (var i = 0; i < feedIds.length; i++) {
    var fc = document.getElementById(feedIds[i]);
    if (fc) {
      article.style.background = 'color-mix(in srgb,var(--accent) 6%,var(--panel))';
      fc.insertBefore(article, fc.firstChild);
      window.scrollTo({top: 0, behavior: 'smooth'});
      if (typeof formatPostTimes === 'function') formatPostTimes(fc);
      htmx.process(article);
      setTimeout(function() {
        article.style.transition = 'background 1.2s ease';
        article.style.background = '';
      }, 50);
      return true;
    }
  }
  return false;
}

// _refreshActiveFeed: triggers an HTMX reload on the visible feed container.
// Used when we don't have post card HTML to prepend (polls, threads).
function _refreshActiveFeed() {
  var feedIds = ['feed-container', 'profile-feed', 'explore-feed', 'thread-container', 'work-replies'];
  for (var i = 0; i < feedIds.length; i++) {
    var fc = document.getElementById(feedIds[i]);
    if (fc) {
      htmx.trigger(fc, 'refreshFeed');
      return;
    }
  }
}

window.updateCharCount = function() {
  var ta = document.getElementById('compose-textarea');
  var ring = document.getElementById('char-ring');
  var inner = document.getElementById('char-count-inner');
  if (!ta || !ring) return;
  var used = ta.value.length;
  var max = 2000;
  var pct = used / max;
  var circumference = 56.5;
  // CHANGE 2: offset formula — empty ring at 0 chars, full at max
  ring.style.strokeDashoffset = String(circumference * (1 - pct));
  var remaining = max - used;
  ring.style.stroke = remaining < 100 ? 'var(--destructive, #ef4444)' : pct > 0.7 ? '#f97316' : 'var(--accent)';
  if (inner) {
    // Show remaining count once the last 20% of the budget is in play.
    var show = used > 1600;
    inner.style.display = show ? 'flex' : 'none';
    if (show) inner.textContent = String(remaining);
  }
};

// Standalone Malkuth-signed poll submit — called from submitCompose when _pollMode is active.
async function submitPollCompose() {
  var ta  = document.getElementById('compose-textarea');
  var btn = document.getElementById('compose-post-btn');
  var question = ta ? ta.value.trim() : '';
  if (!question) { toast('Write a question first', 'error'); return; }
  var options = Array.from(document.querySelectorAll('.poll-option-input'))
    .map(function(i) { return i.value.trim(); }).filter(Boolean);
  if (options.length < 2) { toast('Add at least 2 options', 'error'); return; }
  if (!window.Malkuth || !window._malkuthReady) {
    toast('Signing key unavailable — please reload the page.', 'error');
    return;
  }
  var _mk = await window._malkuthReady;
  if (!_mk || !_mk.hasKey) {
    toast('Signing key unavailable — please reload the page.', 'error');
    return;
  }
  var dur = document.getElementById('poll-duration');
  var durationHours = dur && dur.value ? parseFloat(dur.value) : 24;
  var pollEndsAt = new Date(Date.now() + durationHours * 3600 * 1000).toISOString();
  btn.disabled = true; btn.textContent = 'Posting…';
  try {
    var opts = { poll_options: options, poll_ends_at: pollEndsAt };
    var env = await window.Malkuth.buildSignedWork('post', question, 'poll', [], null, opts);
    var r = await fetch('/events', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'HX-Request': 'true' },
      body: JSON.stringify(env),
    });
    if (r.ok) {
      toast('Poll posted');
      ta.value = '';
      _pollMode = false;
      var panel = document.getElementById('poll-compose-panel');
      if (panel) panel.style.display = 'none';
      var pollBtn = document.getElementById('compose-poll-btn');
      if (pollBtn) pollBtn.classList.remove('active');
      closeCompose();
      _refreshActiveFeed();
    } else { toast(await r.text() || 'Poll failed', 'error'); }
  } catch(e) { toast('Poll failed: ' + e.message, 'error'); }
  btn.disabled = false; btn.textContent = 'Post';
}

window.submitCompose = async function() {
  if (_threadMode) { return submitThreadCompose(); }
  if (_pollMode)   { return submitPollCompose();   }
  var ta  = document.getElementById('compose-textarea');
  var btn = document.getElementById('compose-post-btn');
  var voiceInput = document.getElementById('compose-voice-url');
  var hasVoice = voiceInput && voiceInput.value;
  var hasMedia = document.querySelectorAll('#compose-modal [name="media_urls"]').length > 0
    || !!document.querySelector('#compose-modal input[data-video-asset="1"]');
  if (!ta || (!ta.value.trim() && !hasVoice && !hasMedia)) return;

  // ── Malkuth signed-work path — only path for all compose types ───────────
  if (!window.Malkuth || !window._malkuthReady) {
    toast('Signing key unavailable — please reload the page.', 'error');
    return;
  }
  var _mk = await window._malkuthReady;
  if (!_mk || !_mk.hasKey) {
    toast('Signing key unavailable — please reload the page.', 'error');
    return;
  }

  btn.disabled = true; btn.textContent = 'Posting…';
  try {
    var _wBody = ta.value;

    // Media URLs (images, GIFs)
    var _wMedia = [];
    document.querySelectorAll('#compose-modal [name="media_urls"]').forEach(function(inp) {
      if (inp.value) _wMedia.push(inp.value);
    });

    // Compose-level opts shared across all kinds
    var _wGating = (document.getElementById('compose-gating') || {}).value || 'everyone';
    var _wSubOnly = (document.getElementById('compose-subscribers-only') || {}).value === '1';
    var _wSchedEl = document.getElementById('compose-scheduled-at');
    var _wSched = (_wSchedEl && _wSchedEl.value) ? new Date(_wSchedEl.value).toISOString() : null;
    var _wNSFW = !!(document.getElementById('compose-nsfw') || {}).checked;
    var _wOpts = {
      comment_gating:  _wGating,
      subscriber_only: _wSubOnly,
      scheduled_at:    _wSched,
      is_nsfw:         _wNSFW,
    };

    // Voice opts
    if (hasVoice) {
      var _wVoiceDurEl = document.getElementById('compose-voice-duration');
      _wOpts.voice_url = voiceInput.value;
      _wOpts.voice_duration_secs = _wVoiceDurEl && _wVoiceDurEl.value
        ? parseFloat(_wVoiceDurEl.value) : null;
    }

    // Video opts (hidden fields set by upload flow)
    var _wVidMasterEl = document.querySelector('#compose-modal input[name="video_master_url"]');
    if (_wVidMasterEl && _wVidMasterEl.value) {
      _wOpts.video_master_url = _wVidMasterEl.value;
      var _wVidWmEl = document.querySelector('#compose-modal input[name="video_watermarked_url"]');
      _wOpts.video_watermarked_url = _wVidWmEl && _wVidWmEl.value ? _wVidWmEl.value : null;
      var _wVidPosterEl = document.querySelector('#compose-modal input[name="video_poster_url"]');
      _wOpts.video_poster_url = _wVidPosterEl ? _wVidPosterEl.value || null : null;
      var _wVidDurEl = document.querySelector('#compose-modal input[name="video_duration_secs"]');
      _wOpts.video_duration_secs = _wVidDurEl && _wVidDurEl.value
        ? parseFloat(_wVidDurEl.value) : null;
      var _wVidWEl = document.querySelector('#compose-modal input[name="video_width"]');
      _wOpts.video_width = _wVidWEl && _wVidWEl.value ? parseInt(_wVidWEl.value, 10) || 0 : 0;
      var _wVidHEl = document.querySelector('#compose-modal input[name="video_height"]');
      _wOpts.video_height = _wVidHEl && _wVidHEl.value ? parseInt(_wVidHEl.value, 10) || 0 : 0;
      var _wTusEl = document.querySelector('#compose-modal input[name="tus_upload_id"]');
      if (_wTusEl && _wTusEl.value) _wOpts.tus_upload_id = _wTusEl.value;
    }

    // Read reply parent CID if composing a reply
    var _parentCIDEl = document.querySelector('#compose-modal input[name="parent_cid"]');
    var _parentCID = (_parentCIDEl && _parentCIDEl.value) ? _parentCIDEl.value : null;

    // Read quoted work ID if composing a quote
    var _qwiEl = document.querySelector('#compose-modal input[name="quoted_post_id"]');
    var _quotedWorkID = (_qwiEl && _qwiEl.value) ? _qwiEl.value : null;
    if (_quotedWorkID) _wOpts.quoted_work_id = _quotedWorkID;

    // Determine content kind
    var _wKind = 'post';
    if (hasVoice) { _wKind = 'voice'; }
    else if (_wVidMasterEl && _wVidMasterEl.value) { _wKind = 'video'; }
    else if (_parentCID) { _wKind = 'reply'; }
    else if (_quotedWorkID) { _wKind = 'quote'; }

    var _wEnv = await window.Malkuth.buildSignedWork('post', _wBody, _wKind, _wMedia, _parentCID, _wOpts);
    var _wRes = await fetch('/events', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'HX-Request': 'true' },
      body: JSON.stringify(_wEnv),
    });
    if (_wRes.ok) {
      toast('Post published');
      ta.value = ''; updateCharCount(); clearMediaPreviews(); removeComposeVoice(); removeComposeLocation();
      var _nsfwEl = document.getElementById('compose-nsfw'); if (_nsfwEl) _nsfwEl.checked = false;
      var _piEl = document.querySelector('#compose-modal input[name="parent_id"]');
      if (_piEl && _piEl.value) {
        var _cntEl = document.getElementById('reply-count-' + _piEl.value);
        if (_cntEl) {
          var _newCnt = (parseInt(_cntEl.textContent, 10) || 0) + 1;
          _cntEl.textContent = String(_newCnt);
          _cntEl.style.display = '';
        }
        _piEl.value = '';
      }
      if (_parentCIDEl) _parentCIDEl.value = '';
      closeCompose(); _refreshActiveFeed();
    } else {
      var _wErr = await _wRes.text();
      toast(_wErr || 'Post failed', 'error');
    }
  } catch (_we) { toast('Post failed: ' + _we.message, 'error'); }
  btn.disabled = false; btn.textContent = 'Post';
};

// ── Compose helpers ───────────────────────────────────────────────────────────
window.insertAtSymbol = function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  var pos = ta.selectionStart;
  ta.value = ta.value.slice(0, pos) + '@' + ta.value.slice(pos);
  ta.selectionStart = ta.selectionEnd = pos + 1;
  ta.focus();
  onComposeInput(ta);
};
window.insertHashSymbol = function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  var pos = ta.selectionStart;
  ta.value = ta.value.slice(0, pos) + '#' + ta.value.slice(pos);
  ta.selectionStart = ta.selectionEnd = pos + 1;
  ta.focus();
};
window.insertCashtagSymbol = function() {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  var pos = ta.selectionStart;
  ta.value = ta.value.slice(0, pos) + '$' + ta.value.slice(pos);
  ta.selectionStart = ta.selectionEnd = pos + 1;
  ta.focus();
  ta.dispatchEvent(new Event('input', {bubbles: true}));
};
// CHANGE 4: also exposed as requestComposeLocation for the onclick attribute
window.requestComposeLocation = window.addComposeLocation = function() {
  if (!navigator.geolocation) { toast('Geolocation not supported', 'error'); return; }
  navigator.geolocation.getCurrentPosition(async function(pos) {
    try {
      var r = await fetch('https://nominatim.openstreetmap.org/reverse?lat=' + pos.coords.latitude + '&lon=' + pos.coords.longitude + '&format=json');
      var d = await r.json();
      var city    = (d.address || {}).city || (d.address || {}).town || (d.address || {}).village || '';
      var country = (d.address || {}).country || '';
      var loc = [city, country].filter(Boolean).join(', ');
      if (loc) {
        var label = document.getElementById('compose-location-label');
        var tag   = document.getElementById('compose-location-tag');
        if (label) label.textContent = loc;
        if (tag)   tag.style.display = '';
      }
    } catch(e) { toast('Could not detect location', 'error'); }
  }, function() { toast('Location access denied', 'error'); }, { timeout: 8000 });
};
window.removeComposeLocation = function() {
  var label = document.getElementById('compose-location-label');
  var tag   = document.getElementById('compose-location-tag');
  if (label) label.textContent = '';
  if (tag)   tag.style.display = 'none';
};
window.openGifPicker = function() {
  var existing = document.getElementById('gif-picker-overlay');
  if (existing) { existing.remove(); return; }
  var overlay = document.createElement('div');
  overlay.id = 'gif-picker-overlay';
  overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,.6);z-index:9000;display:flex;align-items:center;justify-content:center';
  overlay.innerHTML = '<div id="gif-picker-box" style="background:var(--surface-2,#1a1a2e);border-radius:16px;width:min(420px,96vw);max-height:80dvh;display:flex;flex-direction:column;overflow:hidden">'
    + '<div style="display:flex;align-items:center;gap:8px;padding:12px 16px;border-bottom:1px solid var(--border,#333)">'
    + '<input id="gif-search-input" type="text" placeholder="Search GIFs…" autocomplete="off" style="flex:1;background:var(--surface-1,#111);border:1px solid var(--border,#333);border-radius:8px;padding:8px 12px;font-size:14px;color:inherit;outline:none">'
    + '<button onclick="document.getElementById(\'gif-picker-overlay\').remove()" style="background:none;border:none;color:var(--text-muted,#888);cursor:pointer;font-size:20px;padding:4px">✕</button>'
    + '</div>'
    + '<div id="gif-results" style="overflow-y:auto;display:grid;grid-template-columns:1fr 1fr;gap:4px;padding:8px"></div>'
    + '</div>';
  document.body.appendChild(overlay);
  overlay.addEventListener('click', function(e){ if (e.target === overlay) overlay.remove(); });
  var input = overlay.querySelector('#gif-search-input');
  var results = overlay.querySelector('#gif-results');
  var timer;
  function searchGifs(q) {
    results.innerHTML = '<div style="grid-column:1/-1;text-align:center;padding:24px;color:var(--text-muted,#888)">' + (q ? 'Searching…' : 'Loading trending GIFs…') + '</div>';
    fetch('/api/gif/search?q='+encodeURIComponent(q)).then(function(r){ return r.json(); }).then(function(d){
      results.innerHTML = '';
      if (!d.results || !d.results.length) {
        results.innerHTML = '<div style="grid-column:1/-1;text-align:center;padding:24px;color:var(--text-muted,#888)">No GIFs found</div>';
        return;
      }
      d.results.forEach(function(g){
        var img = document.createElement('img');
        img.src = g.thumb_url || g.url;
        img.alt = g.title || '';
        img.loading = 'lazy';
        img.style.cssText = 'width:100%;aspect-ratio:1;object-fit:cover;border-radius:8px;cursor:pointer';
        img.addEventListener('click', function(){
          // Replace any previous GIF attachment (one GIF per post)
          document.querySelectorAll('#compose-modal input[data-gif-url]').forEach(function(el){ el.remove(); });
          var prev = document.getElementById('media-previews');
          if (prev) { var old = prev.querySelector('.compose-gif-preview'); if (old) old.remove(); }
          // Store GIF as a media_urls attachment (picked up by submitCompose)
          var modal = document.getElementById('compose-modal');
          if (modal) {
            var h = document.createElement('input');
            h.type = 'hidden'; h.name = 'media_urls'; h.value = g.url;
            h.setAttribute('data-gif-url', '1');
            modal.appendChild(h);
          }
          // Preview tile in the media strip
          if (prev) {
            var wrap = document.createElement('div');
            wrap.className = 'compose-gif-preview';
            var im = document.createElement('img');
            im.src = g.thumb_url || g.url; im.alt = 'GIF';
            wrap.appendChild(im);
            var badge = document.createElement('span');
            badge.className = 'compose-gif-badge'; badge.textContent = 'GIF';
            wrap.appendChild(badge);
            wrap.appendChild(_makeRemoveButton(function(){
              wrap.remove();
              document.querySelectorAll('#compose-modal input[data-gif-url]').forEach(function(el){ el.remove(); });
              _updateMediaGrid();
            }));
            prev.appendChild(wrap);
            _updateMediaGrid();
          }
          overlay.remove();
        });
        results.appendChild(img);
      });
    }).catch(function(){ results.innerHTML = '<div style="grid-column:1/-1;text-align:center;padding:24px;color:var(--text-muted,#888)">Search unavailable</div>'; });
  }
  input.addEventListener('input', function(){
    clearTimeout(timer);
    var q = input.value.trim();
    if (!q) { searchGifs(''); return; }
    timer = setTimeout(function(){ searchGifs(q); }, 350);
  });
  setTimeout(function(){ input.focus(); searchGifs(''); }, 50);
};

// ── Compose: format bar toggle ────────────────────────────────────────────────
window.toggleComposeFormat = function() {
  var bar = document.getElementById('compose-format-bar');
  var btn = document.getElementById('compose-format-btn');
  if (!bar) return;
  var showing = bar.classList.toggle('visible');
  if (btn) btn.classList.toggle('active', showing);
};

// ── Compose: text formatting (wraps selected text or inserts placeholder) ─────
window.composeFormat = function(type) {
  var ta = document.getElementById('compose-textarea');
  if (!ta) return;
  var start = ta.selectionStart, end = ta.selectionEnd;
  var sel   = ta.value.slice(start, end);
  var before = ta.value.slice(0, start), after = ta.value.slice(end);
  var wrapped = '', tailOff = 0;
  switch (type) {
    case 'bold':   wrapped = '**' + (sel || 'bold') + '**';   tailOff = sel ? 0 : 2; break;
    case 'italic': wrapped = '*'  + (sel || 'italic') + '*';  tailOff = sel ? 0 : 1; break;
    case 'ul': {
      var lines = sel ? sel.split('\n') : [''];
      wrapped = lines.map(function(l) { return l ? '• ' + l : '• '; }).join('\n');
      tailOff = 0; break;
    }
    case 'ol': {
      var olLines = sel ? sel.split('\n').filter(function(l) { return l.trim(); }) : [];
      wrapped = olLines.length
        ? olLines.map(function(l, i) { return (i + 1) + '. ' + l; }).join('\n')
        : '1. ';
      tailOff = 0; break;
    }
    default: return;
  }
  ta.value = before + wrapped + after;
  var newEnd = start + wrapped.length - tailOff;
  ta.setSelectionRange(sel ? start : newEnd - (sel ? 0 : (wrapped.length - tailOff - tailOff)), newEnd);
  if (sel) { ta.setSelectionRange(start, start + wrapped.length); }
  ta.focus();
  onComposeInput(ta);
  updateCharCount();
};

// ── Compose: emoji picker ─────────────────────────────────────────────────────
// The works composer reuses the SAME body-level portal picker the messages composer
// uses (_gnEmojiPicker). The in-stack emoji_picker Facet was clipped by the action
// bar's overflow:auto/hidden, so it appeared *inside* the bar; the portal popup is
// position:fixed at body level and floats above the bar like it does for messages.
window.toggleEmojiPicker = function() {
  var btn = document.getElementById('compose-emoji-btn');
  if (!btn) return;
  _gnEmojiPicker(btn, function(em) { insertEmojiIntoCompose(em); });
};
// Keep openEmojiPicker as alias so any legacy call sites still work
window.openEmojiPicker = window.toggleEmojiPicker;

window.insertEmojiIntoCompose = function(em) {
  var ta = document.getElementById('compose-textarea');
  if (ta) {
    var pos = ta.selectionStart;
    ta.value = ta.value.slice(0, pos) + em + ta.value.slice(pos);
    ta.selectionStart = ta.selectionEnd = pos + em.length;
    ta.focus();
    if (typeof updateCharCount === 'function') updateCharCount();
  }
  var wrap = document.querySelector('.compose-emoji-wrap');
  if (wrap) wrap.classList.remove('compose-emoji-wrap--open');
};

// ── Gnosis (messages) composer tools ────────────────────────────────────────────
// The DM composer reuses the SAME pickers/APIs as the works composer (GIF →
// /api/gif/search), but every action targets the .gn-composer-input of the form it
// was fired from — NOT the global #compose-textarea — so plain + sealed threads work.
function _gnInputOf(btn) {
  var form = btn.closest('.gn-composer');
  return form ? form.querySelector('.gn-composer-input') : null;
}
function _gnInsert(input, text) {
  if (!input) return;
  var pos = (input.selectionStart != null) ? input.selectionStart : input.value.length;
  input.value = input.value.slice(0, pos) + text + input.value.slice(pos);
  var np = pos + text.length;
  try { input.selectionStart = input.selectionEnd = np; } catch (e) {}
  input.focus();
}

// GIF — same Klipy-backed API as the works composer; drops the chosen GIF URL into the
// message input so the existing send path delivers it (plain or sealed).
window.gnComposerGif = function(btn) {
  var input = _gnInputOf(btn);
  if (!input) return;
  _gnGifPicker(function(gifUrl) { _gnInsert(input, (input.value ? ' ' : '') + gifUrl + ' '); });
};
function _gnGifPicker(onPick) {
  var ex = document.getElementById('gn-gif-overlay'); if (ex) { ex.remove(); return; }
  var overlay = document.createElement('div');
  overlay.id = 'gn-gif-overlay';
  overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,.6);z-index:10000;display:flex;align-items:center;justify-content:center';
  overlay.innerHTML = '<div style="background:var(--panel,#16161f);border:1px solid var(--border,#333);border-radius:16px;width:min(420px,96vw);max-height:80dvh;display:flex;flex-direction:column;overflow:hidden">'
    + '<div style="display:flex;gap:8px;padding:12px 16px;border-bottom:1px solid var(--border,#333)">'
    + '<input id="gn-gif-search" type="text" placeholder="Search GIFs…" autocomplete="off" style="flex:1;background:var(--surface-1,#111);border:1px solid var(--border,#333);border-radius:8px;padding:8px 12px;color:inherit;outline:none">'
    + '<button type="button" id="gn-gif-close" aria-label="Close" style="background:none;border:none;color:var(--text-muted,#888);font-size:20px;cursor:pointer">✕</button>'
    + '</div><div id="gn-gif-results" style="overflow-y:auto;display:grid;grid-template-columns:1fr 1fr;gap:4px;padding:8px"></div></div>';
  document.body.appendChild(overlay);
  var search = overlay.querySelector('#gn-gif-search');
  var results = overlay.querySelector('#gn-gif-results');
  overlay.addEventListener('click', function(e) { if (e.target === overlay) overlay.remove(); });
  overlay.querySelector('#gn-gif-close').addEventListener('click', function() { overlay.remove(); });
  var timer;
  function run(q) {
    results.innerHTML = '<div style="grid-column:1/-1;text-align:center;padding:24px;color:#888">' + (q ? 'Searching…' : 'Loading trending…') + '</div>';
    fetch('/api/gif/search?q=' + encodeURIComponent(q)).then(function(r) { return r.json(); }).then(function(d) {
      results.innerHTML = '';
      if (!d.results || !d.results.length) { results.innerHTML = '<div style="grid-column:1/-1;text-align:center;padding:24px;color:#888">No GIFs found</div>'; return; }
      d.results.forEach(function(g) {
        var img = document.createElement('img');
        img.src = g.thumb_url || g.url; img.alt = g.title || ''; img.loading = 'lazy';
        img.style.cssText = 'width:100%;aspect-ratio:1;object-fit:cover;border-radius:8px;cursor:pointer';
        img.addEventListener('click', function() { onPick(g.url || g.thumb_url); overlay.remove(); });
        results.appendChild(img);
      });
    }).catch(function() { results.innerHTML = '<div style="grid-column:1/-1;text-align:center;padding:24px;color:#888">Failed to load</div>'; });
  }
  search.addEventListener('input', function() { clearTimeout(timer); timer = setTimeout(function() { run(search.value.trim()); }, 300); });
  run('');
  setTimeout(function() { search.focus(); }, 50);
}

// Emoji — a fixed-position popover that blossoms ABOVE everything (z-index 10000),
// inserting into the message input. No overflow clipping like the in-stack picker.
window.gnComposerEmoji = function(btn) {
  var input = _gnInputOf(btn);
  if (!input) return;
  _gnEmojiPicker(btn, function(em) { _gnInsert(input, em); });
};
function _gnEmojiPicker(anchor, onPick) {
  var ex = document.getElementById('gn-emoji-pop'); if (ex) { ex.remove(); return; }
  var EMOJIS = ['😀','😂','🥹','😍','🥰','😎','🤔','😤','😭','😈','❤️','🧡','💛','💚','💙','💜','🖤','🤍','💔','💯','🔥','✨','⚡','💡','🌙','☀️','🌈','🌊','🦋','🌸','👀','💪','🙏','👏','🎉','🎊','🎵','🎶','🔮','🚀','🍕','☕','🥂','🎯','🏆','🌟','💫','👾','🤖','💀','😏','🫶','🤝','✌️','🫠','🥲','😬','🤯','🥳','💃'];
  var pop = document.createElement('div');
  pop.id = 'gn-emoji-pop';
  pop.style.cssText = 'position:fixed;z-index:10000;background:var(--panel,#16161f);border:1px solid var(--border,#333);border-radius:12px;padding:8px;box-shadow:0 12px 40px rgba(0,0,0,.5);display:grid;grid-template-columns:repeat(8,1fr);gap:2px;max-height:240px;overflow-y:auto;width:300px';
  EMOJIS.forEach(function(em) {
    var b = document.createElement('button');
    b.type = 'button'; b.textContent = em;
    b.style.cssText = 'font-size:20px;line-height:1;padding:5px;background:none;border:none;cursor:pointer;border-radius:6px';
    b.addEventListener('mouseenter', function() { b.style.background = 'var(--panel-hover,#222)'; });
    b.addEventListener('mouseleave', function() { b.style.background = 'none'; });
    b.addEventListener('click', function() { onPick(em); pop.remove(); });
    pop.appendChild(b);
  });
  document.body.appendChild(pop);
  var r = anchor.getBoundingClientRect();
  var top = r.top - pop.offsetHeight - 8;
  if (top < 8) top = r.bottom + 8;
  var left = Math.min(r.left, window.innerWidth - pop.offsetWidth - 8);
  pop.style.top = top + 'px';
  pop.style.left = Math.max(8, left) + 'px';
  setTimeout(function() {
    document.addEventListener('click', function close(e) {
      if (!pop.contains(e.target) && e.target !== anchor) { pop.remove(); document.removeEventListener('click', close); }
    });
  }, 0);
}

// Photo / Camera / Voice — reuse the form's hidden file input (camera + mic use the
// device capture on mobile). Plain threads upload to /messages/send; sealed (E2EE)
// binary attachments are not wired yet, so we say so rather than fail silently.
window.gnComposerMedia  = function(btn) { _gnMediaPick(btn, 'image/*,video/*', null); };
window.gnComposerCamera = function(btn) { _gnMediaPick(btn, 'image/*', 'environment'); };
window.gnComposerVoice  = function(btn) { _gnMediaPick(btn, 'audio/*', 'user'); };
function _gnMediaPick(btn, accept, capture) {
  var form = btn.closest('.gn-composer');
  var inp = form && form.querySelector('.gn-media-input');
  if (!inp) return;
  inp.setAttribute('accept', accept);
  if (capture) inp.setAttribute('capture', capture); else inp.removeAttribute('capture');
  inp.click();
}
window.gnComposerMediaSelected = function(inp) {
  var file = inp.files && inp.files[0];
  var form = inp.closest('.gn-composer');
  if (!file || !form) { inp.value = ''; return; }
  if (form.getAttribute('data-sealed') === '1') {
    if (window.toast) toast('Media in encrypted chats is coming soon');
    inp.value = ''; return;
  }
  var fd = new FormData();
  var cInput = form.querySelector('input[name="c"]');
  fd.append('c', cInput ? cInput.value : '');
  fd.append('body', '');
  fd.append('media', file);
  fetch('/messages/send', { method: 'POST', body: fd, headers: { 'HX-Request': 'true' } })
    .then(function(r) { if (!r.ok) throw new Error('send'); return r.text(); })
    .then(function(html) {
      var list = document.getElementById('gnosis-messages');
      if (list && html) { list.insertAdjacentHTML('beforeend', html); list.scrollTop = list.scrollHeight; }
    })
    .catch(function() { if (window.toast) toast('Could not send media'); })
    .finally(function() { inp.value = ''; });
};

// ── Camera ────────────────────────────────────────────────────────────────────
// The in-app camera is now the Capture Substrate (web/static/js/capture/*). The
// compose action bar's openCameraCapture() entry and the onMediaSelect() output
// handoff are owned by capture.js — nothing camera-related lives in f33d3r.js.

// ── Compose: voice post recording ─────────────────────────────────────────────
var _voiceRecorder = null, _voiceChunks = [], _voiceStart = 0, _voiceTimerID = null, _voiceBlob = null, _voiceAudio = null;
var _voiceAudioCtx = null, _voiceAnalyser = null, _voiceAnimFrame = null;

window.toggleComposeVoice = function() {
  if (_voiceRecorder && _voiceRecorder.state === 'recording') {
    stopComposeVoice(); return;
  }
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    toast('Microphone not supported in this browser', 'error'); return;
  }
  navigator.mediaDevices.getUserMedia({ audio: true })
    .then(function(stream) {
      var mimeType = MediaRecorder.isTypeSupported('audio/webm;codecs=opus') ? 'audio/webm;codecs=opus' :
               MediaRecorder.isTypeSupported('audio/ogg;codecs=opus')  ? 'audio/ogg;codecs=opus'  :
               MediaRecorder.isTypeSupported('audio/mp4')              ? 'audio/mp4'               : '';
      _voiceRecorder = mimeType ? new MediaRecorder(stream, { mimeType }) : new MediaRecorder(stream);
      _voiceChunks = [];
      _voiceRecorder.ondataavailable = function(e) { if (e.data.size > 0) _voiceChunks.push(e.data); };
      _voiceRecorder.onstop = function() {
        stream.getTracks().forEach(function(t) { t.stop(); });
        var mt = _voiceRecorder.mimeType || ''; var ext = mt.includes('ogg') ? 'ogg' : mt.includes('mp4') ? 'mp4' : 'webm';
        _voiceBlob = new Blob(_voiceChunks, { type: _voiceRecorder.mimeType || 'audio/webm' });
        var dur = Math.round((Date.now() - _voiceStart) / 1000);
        _uploadComposeAudio(_voiceBlob, ext, dur);
      };
      _voiceRecorder.start(200);
      _voiceStart = Date.now();
      _startWaveform(stream);
      _showVoiceRecorder();
      var btn = document.getElementById('compose-voice-btn');
      if (btn) btn.classList.add('active');
    })
    .catch(function() { toast('Microphone access denied', 'error'); });
};

window.stopComposeVoice = function() {
  if (_voiceAnimFrame) { cancelAnimationFrame(_voiceAnimFrame); _voiceAnimFrame = null; }
  if (_voiceAudioCtx) { _voiceAudioCtx.close().catch(function(){}); _voiceAudioCtx = null; }
  _voiceAnalyser = null;
  var canvas = document.getElementById('cvr-waveform');
  if (canvas) { var c2 = canvas.getContext('2d'); c2.clearRect(0, 0, canvas.width, canvas.height); }
  if (_voiceRecorder && _voiceRecorder.state !== 'inactive') _voiceRecorder.stop();
  clearInterval(_voiceTimerID); _voiceTimerID = null;
  var rec = document.getElementById('compose-voice-recorder');
  if (rec) rec.classList.remove('active');
  var btn = document.getElementById('compose-voice-btn');
  if (btn) btn.classList.remove('active');
};

function _startWaveform(stream) {
  try {
    _voiceAudioCtx = new (window.AudioContext || window.webkitAudioContext)();
    _voiceAnalyser = _voiceAudioCtx.createAnalyser();
    _voiceAnalyser.fftSize = 64;
    _voiceAnalyser.smoothingTimeConstant = 0.7;
    _voiceAudioCtx.createMediaStreamSource(stream).connect(_voiceAnalyser);
    _drawWaveform();
  } catch(e) {}
}

function _drawWaveform() {
  var canvas = document.getElementById('cvr-waveform');
  if (!canvas || !_voiceAnalyser) return;
  _voiceAnimFrame = requestAnimationFrame(_drawWaveform);
  var dpr = window.devicePixelRatio || 1;
  var W = canvas.offsetWidth, H = canvas.offsetHeight;
  if (!W || !H) return;
  if (canvas.width !== Math.round(W * dpr) || canvas.height !== Math.round(H * dpr)) {
    canvas.width = Math.round(W * dpr);
    canvas.height = Math.round(H * dpr);
  }
  var ctx2d = canvas.getContext('2d');
  ctx2d.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx2d.clearRect(0, 0, W, H);
  var data = new Uint8Array(_voiceAnalyser.frequencyBinCount);
  _voiceAnalyser.getByteFrequencyData(data);
  var barCount = 24, gap = 2;
  var barW = Math.max(2, Math.floor((W - gap * (barCount - 1)) / barCount));
  var accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || '#7c5cfc';
  ctx2d.fillStyle = accent;
  var step = Math.floor(data.length / barCount);
  for (var i = 0; i < barCount; i++) {
    var val = data[i * step] / 255;
    var bh = Math.max(3, val * H * 0.9);
    var x = i * (barW + gap);
    var y = (H - bh) / 2;
    ctx2d.beginPath();
    ctx2d.roundRect(x, y, barW, bh, 2);
    ctx2d.fill();
  }
}

function _showVoiceRecorder() {
  var rec = document.getElementById('compose-voice-recorder');
  if (rec) rec.classList.add('active');
  var timer = document.getElementById('cvr-timer');
  _voiceTimerID = setInterval(function() {
    if (!timer) return;
    var sec = Math.floor((Date.now() - _voiceStart) / 1000);
    timer.textContent = Math.floor(sec / 60) + ':' + String(sec % 60).padStart(2, '0');
  }, 500);
}

async function _uploadComposeAudio(blob, ext, durSecs) {
  var btn = document.getElementById('compose-voice-btn');
  if (btn) { btn.disabled = true; }
  try {
    var fd = new FormData();
    fd.append('audio', blob, 'voice.' + ext);
    var r = await fetch('/upload/voice', { method: 'POST', body: fd });
    if (!r.ok) { toast('Voice upload failed', 'error'); return; }
    var d = await r.json();
    _setComposeVoice(d.url, durSecs);
  } catch(e) { toast('Voice upload failed', 'error'); }
  finally { if (btn) btn.disabled = false; }
}

function _setComposeVoice(url, durSecs) {
  var existing = document.getElementById('compose-voice-url');
  if (!existing) {
    existing = document.createElement('input');
    existing.type = 'hidden'; existing.id = 'compose-voice-url'; existing.name = 'voice_url';
    document.getElementById('compose-modal').appendChild(existing);
  }
  existing.value = url;
  var ed = document.getElementById('compose-voice-duration');
  if (!ed) {
    ed = document.createElement('input');
    ed.type = 'hidden'; ed.id = 'compose-voice-duration'; ed.name = 'voice_duration_secs';
    document.getElementById('compose-modal').appendChild(ed);
  }
  ed.value = durSecs;
  var label = document.getElementById('cvp-label');
  if (label) {
    var m = Math.floor(durSecs / 60), s = durSecs % 60;
    label.textContent = 'Voice post · ' + m + ':' + String(s).padStart(2, '0');
  }
  var prev = document.getElementById('compose-voice-preview');
  if (prev) prev.classList.add('active');
}

window.removeComposeVoice = function() {
  var vi = document.getElementById('compose-voice-url');
  if (vi) vi.value = '';
  var vd = document.getElementById('compose-voice-duration');
  if (vd) vd.value = '';
  _voiceBlob = null;
  var prev = document.getElementById('compose-voice-preview');
  if (prev) prev.classList.remove('active');
  if (_voiceAudio) { _voiceAudio.pause(); _voiceAudio = null; }
  var playBtn = document.getElementById('cvp-play-btn');
  if (playBtn) playBtn.innerHTML = '<svg fill="currentColor" viewBox="0 0 24 24" style="width:13px;height:13px"><polygon points="5,3 19,12 5,21"/></svg>';
};

window.togglePreviewVoice = function() {
  var vi = document.getElementById('compose-voice-url');
  if (!vi || !vi.value) return;
  if (!_voiceAudio) { _voiceAudio = new Audio(vi.value); }
  var playBtn = document.getElementById('cvp-play-btn');
  if (_voiceAudio.paused) {
    _voiceAudio.play();
    if (playBtn) playBtn.innerHTML = '<svg fill="currentColor" viewBox="0 0 24 24" style="width:12px;height:12px"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>';
    _voiceAudio.onended = function() {
      if (playBtn) playBtn.innerHTML = '<svg fill="currentColor" viewBox="0 0 24 24" style="width:13px;height:13px"><polygon points="5,3 19,12 5,21"/></svg>';
      _voiceAudio = null;
    };
  } else {
    _voiceAudio.pause();
    if (playBtn) playBtn.innerHTML = '<svg fill="currentColor" viewBox="0 0 24 24" style="width:13px;height:13px"><polygon points="5,3 19,12 5,21"/></svg>';
  }
};

// ── Post voice player (in feed) ───────────────────────────────────────────────
var _postAudios = {};

window.playPostVoice = function(btn) {
  var player = btn.closest('.post-voice-player');
  if (!player) return;
  var url = player.dataset.audioUrl;
  if (!url) return;
  var aud = _postAudios[url];
  if (!aud) { aud = new Audio(url); _postAudios[url] = aud; }
  if (aud.paused) {
    Object.values(_postAudios).forEach(function(a) { if (a !== aud) a.pause(); });
    aud.play();
    btn.innerHTML = '<svg fill="currentColor" viewBox="0 0 24 24" style="width:14px;height:14px"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>';
    aud.ontimeupdate = function() { _updateVoiceBars(player, aud.currentTime, aud.duration); };
    aud.onended = function() {
      btn.innerHTML = '<svg fill="currentColor" viewBox="0 0 24 24" style="width:14px;height:14px"><polygon points="5,3 19,12 5,21"/></svg>';
      _updateVoiceBars(player, 0, 1);
    };
  } else {
    aud.pause();
    btn.innerHTML = '<svg fill="currentColor" viewBox="0 0 24 24" style="width:14px;height:14px"><polygon points="5,3 19,12 5,21"/></svg>';
  }
};

function _updateVoiceBars(player, currentTime, duration) {
  var wrap = player.querySelector('.voice-bar-wrap');
  if (!wrap) return;
  var bars = wrap.querySelectorAll('.voice-bar');
  if (!bars.length) return;
  var pct = duration > 0 ? currentTime / duration : 0;
  var playedCount = Math.round(pct * bars.length);
  bars.forEach(function(b, i) {
    b.classList.toggle('played', i < playedCount);
  });
}

window.hydrateVoiceBars = function(root) {
  (root || document).querySelectorAll('.post-voice-player').forEach(function(player) {
    var wrap = player.querySelector('.voice-bar-wrap');
    if (!wrap || wrap.children.length) return;
    var N = 28;
    for (var i = 0; i < N; i++) {
      var b = document.createElement('div');
      b.className = 'voice-bar';
      var h = 20 + Math.round(Math.random() * 70);
      b.style.height = h + '%';
      wrap.appendChild(b);
    }
  });
};

// ── Compose: alt text ─────────────────────────────────────────────────────────
window.openAltText = function() {
  var existing = document.getElementById('compose-alttext-row');
  if (existing) { existing.remove(); document.getElementById('compose-alttext-btn').classList.remove('active'); return; }
  var previews = document.getElementById('media-previews');
  var parent   = previews && previews.parentElement;
  if (!parent) { toast('Upload media first', 'info'); return; }
  var row = document.createElement('div');
  row.id = 'compose-alttext-row';
  row.style.cssText = 'display:flex;align-items:center;gap:8px;margin-top:6px';
  var inp = document.createElement('input');
  inp.type = 'text'; inp.name = 'alt_text'; inp.maxLength = 420;
  inp.placeholder = 'Alt text (describe the image for accessibility)…';
  inp.style.cssText = "flex:1;background:var(--panel-hover);border:1.5px solid var(--border);border-radius:var(--radius-sm);padding:7px 12px;color:var(--text-primary);font-size:13px;font-family:'DM Sans',sans-serif;outline:none;transition:border-color 150ms";
  inp.onfocus = function() { inp.style.borderColor = 'var(--accent)'; };
  inp.onblur  = function() { inp.style.borderColor = ''; };
  var close = document.createElement('button');
  close.type = 'button'; close.textContent = '×';
  close.style.cssText = 'background:none;border:none;color:var(--text-muted);cursor:pointer;font-size:18px;padding:0 4px;line-height:1';
  close.onclick = function() { row.remove(); var b = document.getElementById('compose-alttext-btn'); if (b) b.classList.remove('active'); };
  row.appendChild(inp); row.appendChild(close);
  var after = previews.nextSibling;
  if (after) parent.insertBefore(row, after); else parent.appendChild(row);
  inp.focus();
  var btn = document.getElementById('compose-alttext-btn');
  if (btn) btn.classList.add('active');
};

// ── Compose: long post mode ───────────────────────────────────────────────────
var _longPostMode = false;
window.toggleLongPost = function() {
  _longPostMode = !_longPostMode;
  var ta  = document.getElementById('compose-textarea');
  var btn = document.getElementById('compose-longpost-btn');
  if (!ta) return;
  ta.rows = _longPostMode ? 12 : 3;
  ta.style.minHeight = _longPostMode ? '200px' : '';
  if (btn) btn.classList.toggle('active', _longPostMode);
  if (_longPostMode) ta.focus();
};

// ── Adaptive Facet Rendering (AER) for compose ────────────────────────────
// Watches textarea content and transitions compose-card between modes:
//   compose--standard  (default, < 500 chars)
//   compose--long      (500+ chars — expands editor, shows reading time)
//   compose--thread    (2+ paragraphs + 300+ chars — shows thread suggestion)
var ComposeAER = (function () {
    var _card   = null;
    var _ta     = null;
    var _timer  = null;
    var _mode   = 'standard';

    var LONG_THRESHOLD   = 500;  // chars
    var THREAD_CHARS     = 300;  // min chars to show thread suggestion
    var WORDS_PER_MIN    = 200;

    function _el(id) { return document.getElementById(id); }

    function _setMode(mode) {
        if (!_card) return;
        if (_mode === mode) return;
        _mode = mode;
        _card.classList.remove('compose--standard', 'compose--long', 'compose--thread');
        _card.classList.add('compose--' + mode);

        var kindEl  = _el('compose-post-kind');
        var modeEl  = _el('compose-mode-indicator');
        var labelEl = _el('compose-mode-label');
        var rtEl    = _el('compose-reading-time');
        var sugEl   = _el('compose-thread-suggest');

        if (mode === 'standard') {
            if (kindEl)  kindEl.value = 'post';
            if (modeEl)  modeEl.style.display = 'none';
            if (sugEl)   sugEl.style.display   = 'none';
        } else if (mode === 'long') {
            if (kindEl)  kindEl.value = 'long_post';
            if (labelEl) labelEl.textContent = 'Long post';
            var words   = (_ta ? _ta.value.trim().split(/\s+/).length : 0);
            var mins    = Math.max(1, Math.round(words / WORDS_PER_MIN));
            if (rtEl)    rtEl.textContent = '~' + mins + ' min read';
            if (modeEl)  modeEl.style.removeProperty('display');
            if (sugEl)   sugEl.style.display = 'none';
        } else if (mode === 'thread') {
            if (kindEl)  kindEl.value = 'post'; // stays post until user confirms thread
            if (modeEl)  modeEl.style.display = 'none';
            if (sugEl)   sugEl.style.removeProperty('display');
        }
    }

    function _analyze() {
        if (!_ta) return;
        var text  = _ta.value;
        var chars = text.length;

        // Count meaningful paragraphs (double newline or 2+ newlines)
        var paras = text.split(/\n{2,}/).filter(function (p) { return p.trim().length > 20; });

        if (chars >= LONG_THRESHOLD) {
            _setMode('long');
        } else if (paras.length >= 2 && chars >= THREAD_CHARS) {
            _setMode('thread');
        } else {
            _setMode('standard');
        }
    }

    function init() {
        _card   = document.querySelector('.compose-card');
        _ta     = _el('compose-textarea');
        if (!_card || !_ta) return;
        if (_card.classList.contains('compose--standard') ||
            _card.classList.contains('compose--long') ||
            _card.classList.contains('compose--thread')) return; // already initialized

        // Start in standard mode
        _card.classList.add('compose--standard');

        _ta.addEventListener('input', function () {
            clearTimeout(_timer);
            _timer = setTimeout(_analyze, 120); // debounce 120ms
        });
    }

    function reset() {
        _mode = 'standard';
        if (_card) {
            _card.classList.remove('compose--standard', 'compose--long', 'compose--thread');
            _card.classList.add('compose--standard');
        }
        var kindEl = _el('compose-post-kind');
        if (kindEl) kindEl.value = 'post';
        var modeEl = _el('compose-mode-indicator');
        if (modeEl) modeEl.style.display = 'none';
        var sugEl = _el('compose-thread-suggest');
        if (sugEl) sugEl.style.display = 'none';
    }

    return { init: init, reset: reset, analyze: _analyze };
})();

// Convert thread suggestion: split textarea into thread segments
window.convertToThread = function () {
    var ta = document.getElementById('compose-textarea');
    var sugEl = document.getElementById('compose-thread-suggest');
    if (!ta) return;

    var paras = ta.value.split(/\n{2,}/).filter(function (p) { return p.trim(); });
    if (paras.length < 2) return;

    // Put first paragraph back in main textarea
    ta.value = paras[0];
    if (sugEl) sugEl.style.display = 'none';
    ComposeAER.reset();

    // Activate thread mode
    var threadBtn = document.getElementById('compose-thread-btn');
    if (threadBtn) {
        // Ensure thread mode is on (toggles if currently off)
        if (!_threadMode) threadBtn.click();
        // Add remaining paragraphs as thread segments after a tick
        setTimeout(function () {
            paras.slice(1).forEach(function (para) {
                var addBtn = document.getElementById('thread-add-btn');
                if (addBtn) {
                    addBtn.click();
                    // Find the last thread textarea and fill it
                    var segments = document.querySelectorAll('.thread-seg-ta');
                    if (segments.length) {
                        segments[segments.length - 1].value = para;
                    }
                }
            });
        }, 50);
    }
};

// ── Compose: scheduler ────────────────────────────────────────────────────────
var _scheduleAt = null;
window.openScheduler = function() {
  var existing = document.getElementById('compose-schedule-panel');
  if (existing) { existing.remove(); return; }
  var panel = document.createElement('div');
  panel.id = 'compose-schedule-panel';
  panel.style.cssText = 'border-top:1px solid var(--border-soft);padding:12px 16px;background:var(--panel)';
  var now = new Date(); now.setMinutes(now.getMinutes() + 30);
  var minVal = now.toISOString().slice(0, 16);
  panel.innerHTML =
    '<p style="font-size:11px;font-weight:700;color:var(--text-secondary);text-transform:uppercase;letter-spacing:.06em;margin:0 0 10px">Schedule post</p>' +
    '<div style="display:flex;align-items:center;gap:10px">' +
      '<input id="schedule-dt-input" type="datetime-local" min="' + minVal + '" ' +
        'style="flex:1;background:var(--panel-hover);border:1.5px solid var(--border);border-radius:var(--radius-sm);padding:8px 12px;color:var(--text-primary);font-size:13px;font-family:inherit;outline:none;color-scheme:dark light;min-width:0">' +
      '<button id="schedule-set-btn" style="background:var(--accent);color:#fff;border:none;border-radius:var(--radius-full);padding:8px 18px;font-size:13px;font-weight:600;font-family:inherit;cursor:pointer;white-space:nowrap;flex-shrink:0">Set</button>' +
      '<button id="schedule-clear-btn" style="background:none;color:var(--text-muted);border:1px solid var(--border);border-radius:var(--radius-full);padding:8px 12px;font-size:13px;font-family:inherit;cursor:pointer;flex-shrink:0">Clear</button>' +
    '</div>';
  var card = document.getElementById('compose-card');
  if (card) card.appendChild(panel);
  var dtInp = document.getElementById('schedule-dt-input');
  if (_scheduleAt && dtInp) dtInp.value = _scheduleAt;
  var schedBtn = document.getElementById('compose-schedule-btn');
  document.getElementById('schedule-set-btn').onclick = function() {
    var val = dtInp ? dtInp.value : '';
    if (!val) { toast('Pick a date and time', 'error'); return; }
    _scheduleAt = val;
    var hi = document.getElementById('compose-scheduled-at');
    if (hi) hi.value = new Date(val).toISOString();
    panel.remove();
    if (schedBtn) { schedBtn.classList.add('active'); schedBtn.title = 'Scheduled: ' + new Date(val).toLocaleString([], {dateStyle:'medium',timeStyle:'short',hour12:false}); }
    // Show the schedule chip in the compose body
    var disp = document.getElementById('compose-schedule-display');
    var lbl  = document.getElementById('compose-schedule-label');
    if (lbl) lbl.textContent = new Date(val).toLocaleString([], {month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',hour12:false});
    if (disp) disp.style.display = '';
    toast('Scheduled for ' + new Date(val).toLocaleString([], {month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',hour12:false}));
  };
  document.getElementById('schedule-clear-btn').onclick = function() {
    _scheduleAt = null;
    var hi = document.getElementById('compose-scheduled-at'); if (hi) hi.remove();
    panel.remove();
    if (schedBtn) { schedBtn.classList.remove('active'); schedBtn.title = 'Schedule'; }
  };
};

// clearComposeSchedule — removes the schedule chip and resets the schedule button
window.clearComposeSchedule = function() {
  _scheduleAt = null;
  var hi = document.getElementById('compose-scheduled-at'); if (hi) hi.value = '';
  var disp = document.getElementById('compose-schedule-display');
  if (disp) disp.style.display = 'none';
  var sb = document.getElementById('compose-schedule-btn');
  if (sb) { sb.classList.remove('active'); sb.title = 'Schedule post'; }
};

// ── Compose: audience sheet ───────────────────────────────────────────────────
window.openAudienceSheet = function() {
  var existing = document.getElementById('compose-audience-panel');
  if (existing) { existing.remove(); return; }
  var panel = document.createElement('div');
  panel.id = 'compose-audience-panel';
  panel.style.cssText = 'border-top:1px solid var(--border-soft);padding:12px 16px;background:var(--panel)';
  var hi     = document.getElementById('compose-audience');
  var current = hi ? hi.value : 'public';
  var opts = [
    { val:'public',    icon:'🌐', label:'Public',       desc:'Visible to everyone' },
    { val:'followers', icon:'👥', label:'Followers only', desc:'Only people who follow you' },
    { val:'circle',    icon:'🔵', label:'Close circle',  desc:'Your inner circle' },
    { val:'relay_off', icon:'🚫', label:'No relay',      desc:'Prevent reposts by others' },
  ];
  panel.innerHTML = '<p style="font-size:11px;font-weight:700;color:var(--text-secondary);text-transform:uppercase;letter-spacing:.06em;margin:0 0 10px">Audience</p>' +
    opts.map(function(o) {
      return '<label style="display:flex;align-items:center;gap:12px;padding:9px 0;cursor:pointer;border-bottom:1px solid var(--border-soft)">' +
        '<input type="radio" name="_audience_pick" value="' + o.val + '"' + (o.val === current ? ' checked' : '') + ' style="accent-color:var(--accent);width:15px;height:15px;flex-shrink:0">' +
        '<span style="font-size:18px">' + o.icon + '</span>' +
        '<div style="min-width:0"><p style="font-size:14px;font-weight:600;color:var(--text-primary);margin:0">' + o.label + '</p>' +
        '<p style="font-size:12px;color:var(--text-muted);margin:0">' + o.desc + '</p></div>' +
        '</label>';
    }).join('') +
    '<button id="audience-apply-btn" style="margin-top:12px;background:var(--accent);color:#fff;border:none;border-radius:var(--radius-full);padding:9px 0;font-size:13px;font-weight:700;font-family:inherit;cursor:pointer;width:100%">Apply</button>';
  var card = document.getElementById('compose-card');
  if (card) card.appendChild(panel);
  document.getElementById('audience-apply-btn').onclick = function() {
    var picked = panel.querySelector('input[name="_audience_pick"]:checked');
    var val = picked ? picked.value : 'public';
    if (!hi) { hi = document.createElement('input'); hi.type='hidden'; hi.name='audience'; hi.id='compose-audience'; document.getElementById('compose-modal').appendChild(hi); }
    hi.value = val;
    panel.remove();
    var labels = { public:'Everyone', followers:'Followers', circle:'Circle', relay_off:'No relay' };
    var lbl = document.getElementById('compose-audience-label');
    if (lbl) lbl.textContent = labels[val] || val;
    var audienceBtn = document.getElementById('compose-audience-btn');
    if (audienceBtn) audienceBtn.classList.toggle('active', val !== 'public');
  };
};

// ── Compose: subscribers-only ─────────────────────────────────────────────────
var _subscribersOnly = false;
window.toggleSubscribersOnly = function() {
  _subscribersOnly = !_subscribersOnly;
  var btn = document.getElementById('compose-visibility-btn');
  if (btn) btn.classList.toggle('active', _subscribersOnly);
  var hi = document.getElementById('compose-subscribers-only');
  if (_subscribersOnly) {
    if (!hi) { hi = document.createElement('input'); hi.type='hidden'; hi.name='subscribers_only'; hi.value='1'; hi.id='compose-subscribers-only'; document.getElementById('compose-modal').appendChild(hi); }
    toast('Subscribers only');
  } else {
    if (hi) hi.remove();
    toast('Visible to all');
  }
};

// ── Compose: undo timer ───────────────────────────────────────────────────────
var _undoTimerSecs = 0;
window.toggleUndoTimer = function() {
  var steps = [0, 10, 30, 60];
  var idx = steps.indexOf(_undoTimerSecs);
  _undoTimerSecs = steps[(idx + 1) % steps.length];
  var btn = document.getElementById('compose-undo-timer-btn');
  var hi  = document.getElementById('compose-undo-delay');
  if (_undoTimerSecs > 0) {
    if (!hi) { hi = document.createElement('input'); hi.type='hidden'; hi.name='undo_delay_secs'; hi.id='compose-undo-delay'; document.getElementById('compose-modal').appendChild(hi); }
    hi.value = String(_undoTimerSecs);
    if (btn) { btn.classList.add('active'); btn.title = 'Undo timer: ' + _undoTimerSecs + 's (click to change)'; }
    toast('Undo timer: ' + _undoTimerSecs + 's');
  } else {
    if (hi) hi.remove();
    if (btn) { btn.classList.remove('active'); btn.title = 'Undo timer (delay before post goes live)'; }
    toast('Undo timer off');
  }
};

// ── Compose: community selector ───────────────────────────────────────────────
window.openCommunitySelector = function() {
  var existing = document.getElementById('compose-community-panel');
  if (existing) { existing.remove(); return; }
  var panel = document.createElement('div');
  panel.id = 'compose-community-panel';
  panel.style.cssText = 'border-top:1px solid var(--border-soft);padding:12px 16px;background:var(--panel)';
  panel.innerHTML =
    '<p style="font-size:11px;font-weight:700;color:var(--text-secondary);text-transform:uppercase;letter-spacing:.06em;margin:0 0 10px">Post to Community</p>' +
    '<input id="community-search-inp" type="search" placeholder="Search communities…" autocomplete="off" ' +
      'style="width:100%;background:var(--panel-hover);border:1.5px solid var(--border);border-radius:var(--radius-sm);padding:8px 12px;color:var(--text-primary);font-size:13px;font-family:inherit;outline:none;box-sizing:border-box;transition:border-color 150ms">' +
    '<p id="community-search-hint" style="font-size:12px;color:var(--text-muted);margin:10px 0 0">Communities launch in Sprint 2 — post will be public for now.</p>';
  var card = document.getElementById('compose-card');
  if (card) card.appendChild(panel);
  var inp = document.getElementById('community-search-inp');
  if (inp) { inp.focus(); inp.onfocus = function() { inp.style.borderColor='var(--accent)'; }; inp.onblur = function() { inp.style.borderColor=''; }; }
};

// ── Compose: reset all action-bar state on close ──────────────────────────────
(function() {
  var _origClose = window.closeCompose;
  window.closeCompose = function() {
    _origClose && _origClose();
    _scheduleAt = null; _subscribersOnly = false; _undoTimerSecs = 0; _longPostMode = false;
    // Remove all hidden state fields
    var qpi = document.querySelector('#compose-modal input[name="quoted_post_id"]'); if (qpi) qpi.remove();
    var qpv = document.getElementById('compose-quote-preview'); if (qpv) qpv.remove();
    ['compose-scheduled-at','compose-audience','compose-subscribers-only','compose-undo-delay'].forEach(function(id) {
      var el = document.getElementById(id); if (el) el.remove();
    });
    // Remove all inline compose panels
    ['compose-schedule-panel','compose-audience-panel','compose-community-panel','compose-alttext-row'].forEach(function(id) {
      var el = document.getElementById(id); if (el) el.remove();
    });
    // Reset format bar
    var fb = document.getElementById('compose-format-bar');
    if (fb) fb.classList.remove('visible');
    // Reset long post
    var ta = document.getElementById('compose-textarea');
    if (ta) { ta.rows = 3; ta.style.minHeight = ''; }
    // Reset active states on all action buttons
    ['compose-schedule-btn','compose-audience-btn','compose-visibility-btn','compose-undo-timer-btn','compose-longpost-btn','compose-alttext-btn','compose-format-btn'].forEach(function(id) {
      var el = document.getElementById(id); if (el) { el.classList.remove('active'); }
    });
    // Reset schedule button title
    var sb = document.getElementById('compose-schedule-btn'); if (sb) sb.title = 'Schedule';
    var emojiWrap = document.querySelector('.compose-emoji-wrap');
    if (emojiWrap) emojiWrap.classList.remove('compose-emoji-wrap--open');
    // AER: reset compose mode on close
    ComposeAER.reset();
  };
})();


// ── Caret-anchored dropdown positioning ───────────────────────────────────────
// Places an autocomplete dropdown directly under the caret LINE of a textarea,
// instead of letting CSS anchor it to the bottom of the (tall, growing) textarea.
// Uses an offscreen mirror with identical typography to measure the caret's Y.
// The dropdown keeps its CSS left/right (full width); only `top` is set here.
window.positionDropdownAtCaret = function(ta, dd) {
  if (!ta || !dd) return;
  var cs = getComputedStyle(ta);
  var mirror = document.createElement('div');
  ['fontFamily','fontSize','fontWeight','fontStyle','letterSpacing','lineHeight',
   'textTransform','wordSpacing','textIndent','paddingTop','paddingRight',
   'paddingBottom','paddingLeft','borderTopWidth','borderRightWidth',
   'borderBottomWidth','borderLeftWidth'].forEach(function(p){ mirror.style[p] = cs[p]; });
  mirror.style.position = 'absolute';
  mirror.style.visibility = 'hidden';
  mirror.style.left = '-9999px';
  mirror.style.top = '0';
  mirror.style.width = ta.clientWidth + 'px';
  mirror.style.height = 'auto';
  mirror.style.whiteSpace = 'pre-wrap';
  mirror.style.wordWrap = 'break-word';
  mirror.style.boxSizing = 'border-box';
  mirror.textContent = ta.value.slice(0, ta.selectionStart);
  var marker = document.createElement('span');
  marker.textContent = '​'; // zero-width marker at the caret
  mirror.appendChild(marker);
  document.body.appendChild(mirror);
  var lineH = parseFloat(cs.lineHeight) || (parseFloat(cs.fontSize) * 1.4);
  var top = marker.offsetTop + lineH - ta.scrollTop + 4;
  document.body.removeChild(mirror);
  dd.style.top = (top < 0 ? 0 : top) + 'px';
};

// ── Mention autocomplete ──────────────────────────────────────────────────────
var _mentionTimer = null;
window.onComposeInput = function(ta) {
  var dd = document.getElementById('mention-dropdown');
  var cursor = ta.selectionStart;
  var text = ta.value.slice(0, cursor);
  var match = text.match(/@([a-zA-Z0-9_\-]*)$/);
  if (!match) { if (dd) dd.style.display = 'none'; return; }
  var q = match[1];
  clearTimeout(_mentionTimer);
  _mentionTimer = setTimeout(async function() {
    if (q.length < 1) { dd.style.display = 'none'; return; }
    try {
      var res = await fetch('/api/users/search?q=' + encodeURIComponent(q));
      var users = await res.json();
      if (!users || !users.length) { dd.style.display = 'none'; return; }
      var myHandle = (document.body.dataset.handle || '').toLowerCase();
      var filtered = users.filter(function(u) { return !myHandle || u.handle.toLowerCase() !== myHandle; });
      if (!filtered.length) { dd.style.display = 'none'; return; }
      dd.innerHTML = filtered.map(function(u, i) {
        return '<button type="button" data-mention-idx="' + i + '" onclick="insertMention(\'@' + u.handle + '\')" style="display:flex;align-items:center;gap:10px;width:100%;padding:10px 14px;background:none;border:none;border-bottom:1px solid var(--border);cursor:pointer;text-align:left"><div style="width:30px;height:30px;border-radius:50%;overflow:hidden;flex-shrink:0;' + (u.avatar_url ? '' : 'background:linear-gradient(135deg,var(--accent),color-mix(in srgb,var(--accent) 60%,#000));display:flex;align-items:center;justify-content:center;color:#fff;font-size:12px;font-weight:700') + '">' + (u.avatar_url ? '<img src="' + u.avatar_url + '" style="width:100%;height:100%;object-fit:cover">' : u.handle[0].toUpperCase()) + '</div><div><p style="font-size:13px;font-weight:500;color:var(--text-primary);margin:0">' + u.display_name + '</p><p style="font-size:11px;color:var(--text-muted);margin:0">@' + u.handle + '</p></div></button>';
      }).join('');
      window.positionDropdownAtCaret(ta, dd);
      dd.style.display = 'block';
    } catch(e) { dd.style.display = 'none'; }
  }, 180);
};
window.insertMention = function(handle) {
  var ta = document.getElementById('compose-textarea');
  var dd = document.getElementById('mention-dropdown');
  if (!ta) return;
  var cursor  = ta.selectionStart;
  var before  = ta.value.slice(0, cursor);
  var after   = ta.value.slice(cursor);
  var replaced = before.replace(/@[a-zA-Z0-9_\-]*$/, handle + ' ');
  ta.value = replaced + after;
  ta.selectionStart = ta.selectionEnd = replaced.length;
  ta.focus();
  if (dd) dd.style.display = 'none';
  // Refresh the highlight backdrop (HTMX) after the programmatic edit.
  ta.dispatchEvent(new Event('input', { bubbles: true }));
  updateCharCount();
};
document.addEventListener('keydown', function(e) {
  var dd = document.getElementById('mention-dropdown');
  if (!dd || dd.style.display === 'none') return;
  var btns = Array.from(dd.querySelectorAll('button[data-mention-idx]'));
  if (!btns.length) return;
  var active = dd.querySelector('button.mention-active');
  var idx = active ? parseInt(active.dataset.mentionIdx) : -1;
  if      (e.key === 'ArrowDown')          { e.preventDefault(); idx = Math.min(idx + 1, btns.length - 1); }
  else if (e.key === 'ArrowUp')            { e.preventDefault(); idx = Math.max(idx - 1, 0); }
  else if (e.key === 'Enter')              { e.preventDefault(); (active || btns[0])?.click(); return; }
  else if (e.key === 'Escape')             { dd.style.display = 'none'; return; }
  else return;
  btns.forEach(function(b) { b.classList.remove('mention-active'); b.style.background = ''; });
  btns[idx].classList.add('mention-active');
  btns[idx].style.background = 'var(--panel-hover)';
});
document.addEventListener('click', function(e) {
  var dd = document.getElementById('mention-dropdown');
  if (dd && !dd.contains(e.target)) dd.style.display = 'none';
});

// ── Pre-upload hash checks ────────────────────────────────────────────────────
// Browser computes SHA-256 of the file and posts only the hex to a lightweight
// check endpoint. If the server returns a duplicate the file never leaves the device.
// Falls back silently if crypto.subtle is unavailable (plain-HTTP dev environments).

async function _preCheckVideoHash(file, vWrap, vModal, vPostBtn) {
  if (!window.crypto || !window.crypto.subtle) return false;
  try {
    var buf = await file.arrayBuffer();
    var hashBuf = await crypto.subtle.digest('SHA-256', buf);
    var hex = Array.from(new Uint8Array(hashBuf)).map(function(b) { return b.toString(16).padStart(2, '0'); }).join('');
    var fd = new FormData();
    fd.append('sha256', hex);
    var res = await fetch('/upload/check-video-hash', { method: 'POST', body: fd });
    if (!res.ok) return false;
    var data = await res.json();
    if (data.status === 'duplicate' || data.status === 'self_duplicate') {
      if (data.status === 'self_duplicate') {
        _showSelfDuplicateWarning(vModal);
      } else {
        _showDuplicateVideoWarning(data.original_handle, data.original_post_id, vModal);
      }
      if (data.master_url) {
        _stashVideoFields(vModal, {
          master_url:            data.master_url,
          watermarked_mp4_url: data.watermarked_mp4_url || '',
          poster_url:            data.poster_url || '',
          duration_secs:         data.duration_secs || 0,
          width:                 data.width || 0,
          height:                data.height || 0
        });
        _renderVideoTile(vWrap, { master_url: data.master_url, poster_url: data.poster_url || '' }, vModal);
      } else {
        vWrap.remove();
      }
      if (vPostBtn) { vPostBtn.disabled = false; vPostBtn.textContent = 'Post'; }
      return true;
    }
    return false;
  } catch (e) { return false; }
}

async function _preCheckImageHash(file, wrap, modal) {
  if (!window.crypto || !window.crypto.subtle) return false;
  try {
    var buf = await file.arrayBuffer();
    var hashBuf = await crypto.subtle.digest('SHA-256', buf);
    var hex = Array.from(new Uint8Array(hashBuf)).map(function(b) { return b.toString(16).padStart(2, '0'); }).join('');
    var fd = new FormData();
    fd.append('sha256', hex);
    var res = await fetch('/upload/check-image-hash', { method: 'POST', body: fd });
    if (!res.ok) return false;
    var data = await res.json();
    if (data.status === 'duplicate' && data.url) {
      _showDuplicateVideoWarning(data.original_handle || '', '', modal);
      _stashMediaURL(modal, data.url);
      _renderImageTile(wrap, data.url, modal);
      return true;
    }
    return false;
  } catch (e) { return false; }
}

// ── Media upload ──────────────────────────────────────────────────────────────
// The /upload/post-media endpoint returns one of:
//   image: { kind: "image", url }
//   video: { kind: "video", master_url, poster_url, duration_secs, width, height }
// (Sprint 0 / S0.6.) Videos block on Caeor's HLS transcode — show a spinner.

window.onMediaSelect = async function(input) {
  var files = Array.from(input.files);
  var modal = document.getElementById('compose-modal');
  var prev  = document.getElementById('media-previews');
  var postBtn = document.getElementById('compose-post-btn');
  var isVideo = function(f) { return /^video\//i.test(f.type) || /\.(mp4|mov|webm|mkv|m4v)$/i.test(f.name); };

  for (var i = 0; i < files.length; i++) {
    var file = files[i];
    var video = isVideo(file);

    // Lock Post button and show upload status in the compose area.
    if (postBtn) { postBtn.disabled = true; postBtn.textContent = video ? 'Uploading…' : 'Uploading…'; }

    // Placeholder tile with spinner — shows immediately so user sees activity.
    var wrap = document.createElement('div');
    wrap.className = 'compose-media-item';
    wrap.innerHTML = '<div style="display:flex;flex-direction:column;align-items:center;justify-content:center;gap:4px;width:100%;min-height:80px">'
      + '<div style="width:22px;height:22px;border:2px solid var(--border);border-top-color:var(--accent);border-radius:50%;animation:spin 0.7s linear infinite"></div>'
      + (video ? '<span style="font-size:8px;font-weight:700;letter-spacing:.05em;color:var(--accent);text-transform:uppercase">0%</span>' : '')
      + '</div>';
    if (prev) { prev.appendChild(wrap); _updateMediaGrid(); }

    // Facet(video_upload) — client-side hash check runs first (crypto.subtle); falls back
    // to server-side dedup inside uploadPostMedia if unavailable (HTTP dev environments).
    if (video) {
      (async function(vFile, vWrap, vModal, vPostBtn, vFilename) {
        // Pre-upload hash check: if the file is a known duplicate, skip the full upload.
        var handled = await _preCheckVideoHash(vFile, vWrap, vModal, vPostBtn);
        if (handled) return;

        var xhr = new XMLHttpRequest();
        var fd = new FormData();
        fd.append('media', vFile);

        xhr.upload.onprogress = function(e) {
          if (!e.lengthComputable) return;
          var pct = Math.round(e.loaded / e.total * 100);
          var span = vWrap.querySelector('span');
          if (span) span.textContent = pct + '%';
        };

        xhr.onload = function() {
          if (xhr.status !== 200) {
            vWrap.remove();
            toast('Upload failed (' + xhr.status + '): ' + vFilename, 'error');
            if (vPostBtn) { vPostBtn.disabled = false; vPostBtn.textContent = 'Post'; }
            return;
          }
          var data;
          try { data = JSON.parse(xhr.responseText); } catch(e) {
            vWrap.remove();
            toast('Upload parse error: ' + vFilename, 'error');
            if (vPostBtn) { vPostBtn.disabled = false; vPostBtn.textContent = 'Post'; }
            return;
          }
          // Server-side dedup result — same response format as image dedup.
          if (data.status === 'duplicate' || data.status === 'self_duplicate') {
            if (data.status === 'self_duplicate') {
              _showSelfDuplicateWarning(vModal);
            } else {
              _showDuplicateVideoWarning(data.original_handle, data.original_post_id, vModal);
            }
            if (data.master_url) {
              _stashVideoFields(vModal, {
                master_url:            data.master_url,
                watermarked_mp4_url: data.watermarked_mp4_url || '',
                poster_url:            data.poster_url || '',
                duration_secs:         data.duration_secs || 0,
                width:                 data.width || 0,
                height:                data.height || 0
              });
              _renderVideoTile(vWrap, { master_url: data.master_url, poster_url: data.poster_url || '' }, vModal);
            } else {
              vWrap.remove();
            }
            if (vPostBtn) { vPostBtn.disabled = false; vPostBtn.textContent = 'Post'; }
            return;
          }
          if (data.kind === 'video_queued' && data.upload_id) {
            var span = vWrap.querySelector('span');
            if (span) span.textContent = 'Processing…';
            _pollTusStatus(data.upload_id, vWrap, vModal, vPostBtn, vFilename);
          } else if (data.kind === 'video' && data.master_url) {
            _stashVideoFields(vModal, data);
            _renderVideoTile(vWrap, data, vModal);
            if (vPostBtn) { vPostBtn.disabled = false; vPostBtn.textContent = 'Post'; }
          } else {
            vWrap.remove();
            toast('Unexpected upload response: ' + vFilename, 'error');
            if (vPostBtn) { vPostBtn.disabled = false; vPostBtn.textContent = 'Post'; }
          }
        };

        xhr.onerror = function() {
          vWrap.remove();
          toast('Network error uploading: ' + vFilename, 'error');
          if (vPostBtn) { vPostBtn.disabled = false; vPostBtn.textContent = 'Post'; }
        };

        xhr.open('POST', '/upload/post-media');
        xhr.send(fd);
      })(file, wrap, modal, postBtn, file.name);

      if (postBtn) { postBtn.disabled = true; postBtn.textContent = 'Uploading…'; }
      input.value = '';
      return;
    }

    // Image path: pre-upload hash check first, then fall through to server upload.
    var imgHandled = await _preCheckImageHash(file, wrap, modal);
    if (imgHandled) continue;

    var fd = new FormData();
    fd.append('media', file);
    try {
      var res = await fetch('/upload/post-media', { method: 'POST', body: fd });
      if (!res.ok) {
        wrap.remove();
        var errText = await res.text().catch(function() { return ''; });
        toast((errText || 'Upload failed') + ': ' + file.name, 'error');
        continue;
      }
      var data = await res.json();
      var kind = data.kind || (data.url ? 'image' : 'unknown');

      if (data.status === 'self_duplicate') {
        _showSelfDuplicateWarning(modal);
        if (data.url) { _stashMediaURL(modal, data.url); _renderImageTile(wrap, data.url, modal); }
        else { wrap.remove(); }
      } else if (data.status === 'duplicate') {
        _showDuplicateVideoWarning(data.original_handle, data.original_post_id, modal);
        if (data.url) { _stashMediaURL(modal, data.url); _renderImageTile(wrap, data.url, modal); }
        else { wrap.remove(); }
      } else if (kind === 'image' || data.url) {
        _stashMediaURL(modal, data.url);
        _renderImageTile(wrap, data.url, modal);
      } else {
        wrap.remove();
        toast('Upload returned an unexpected response', 'error');
      }
    } catch (e) {
      wrap.remove();
      toast('Upload failed: ' + file.name, 'error');
    }
  }

  // Restore Post button regardless of outcome (only for non-video / sync paths).
  if (postBtn) { postBtn.disabled = false; postBtn.textContent = 'Post'; }
  input.value = '';
};

async function _pollTusStatus(uploadId, wrap, modal, postBtn, filename) {
  var maxTries = 360; // 30 minutes at 5s intervals
  for (var i = 0; i < maxTries; i++) {
    await new Promise(function(r) { setTimeout(r, 5000); });
    // Only catch network/parse errors — never swallow post-processing errors
    var job = null;
    try {
      var r = await fetch('/upload/tus/status/' + uploadId);
      if (!r.ok) continue;
      job = await r.json();
    } catch(e) { continue; }

    if (job.status === 'duplicate') {
      // Video already exists on F33D3R — show warning, reference canonical media.
      // Response shape: {"status":"duplicate","original":{master_url,poster_url,...}}
      var orig = job.original || {};
      _showDuplicateVideoWarning(orig.creator_handle || '', orig.work_id || '', modal);
      if (orig.master_url) {
        _stashVideoFields(modal, {
          master_url:          orig.master_url,
          watermarked_mp4_url: orig.watermarked_mp4_url || '',
          poster_url:          orig.poster_url || '',
          duration_secs:       orig.duration_secs || 0,
          width:               orig.width || 0,
          height:              orig.height || 0
        });
        _renderVideoTile(wrap, { master_url: orig.master_url, poster_url: orig.poster_url || '' }, modal);
      }
      // If master_url is empty the tile stays as-is — server lineage redirect
      // will apply the correct URL when the post is submitted.
      if (postBtn) { postBtn.disabled = false; postBtn.textContent = 'Post'; }
      return;
    } else if (job.status === 'ready' && job.output) {
      _stashVideoFields(modal, {
        master_url:            job.output.master_url,
        watermarked_mp4_url: job.output.watermarked_mp4_url || '',
        poster_url:            job.output.poster_url || '',
        duration_secs:         job.output.duration_seconds || 0,
        width:                 job.output.source_width || 0,
        height:                job.output.source_height || 0,
        upload_id:             uploadId
      });
      _renderVideoTile(wrap, { master_url: job.output.master_url, poster_url: job.output.poster_url }, modal);
      if (postBtn) { postBtn.disabled = false; postBtn.textContent = 'Post'; }
      return;
    } else if (job.status === 'failed') {
      wrap.remove();
      toast('Video processing failed: ' + filename, 'error');
      if (postBtn) { postBtn.disabled = false; postBtn.textContent = 'Post'; }
      return;
    } else {
      var spinner = wrap.querySelector('span');
      if (spinner) spinner.textContent = 'Processing…';
    }
  }
  wrap.remove();
  toast('Video processing timed out: ' + filename, 'error');
  if (postBtn) { postBtn.disabled = false; postBtn.textContent = 'Post'; }
}

// ── Hidden-field helpers (one source of truth for compose state) ─────────────

function _stashMediaURL(modal, url) {
  if (!url || url === 'undefined' || url === 'null') return;
  var h = document.createElement('input');
  h.type = 'hidden'; h.name = 'media_urls'; h.value = url;
  h.dataset.mediaUrl = url;
  modal.appendChild(h);
}

function _stashVideoFields(modal, data) {
  // Only one video per post for now — replace any existing.
  modal.querySelectorAll('input[data-video-asset="1"]').forEach(function (n) { n.remove(); });
  var pairs = [
    ['video_master_url',      data.master_url],
    ['video_watermarked_url', data.watermarked_mp4_url || ''],
    ['video_poster_url',      data.poster_url || ''],
    ['video_duration_secs',   String(data.duration_secs || 0)],
    ['video_width',           String(data.width || 0)],
    ['video_height',          String(data.height || 0)],
    ['tus_upload_id',         data.upload_id || ''],
  ];
  pairs.forEach(function (kv) {
    var h = document.createElement('input');
    h.type = 'hidden'; h.name = kv[0]; h.value = kv[1];
    h.dataset.videoAsset = '1';
    modal.appendChild(h);
  });
}

function _renderImageTile(wrap, url, modal) {
  wrap.innerHTML = '<img src="' + url + '" alt="">';
  wrap.appendChild(_makeRemoveButton(function () {
    var h = modal.querySelector('[data-media-url="' + url + '"]');
    if (h) h.remove();
    wrap.remove();
    _updateMediaGrid();
    _syncAltBtn();
  }));
  _syncAltBtn();
}

function _renderVideoTile(wrap, data, modal) {
  var poster = data.poster_url || '';
  wrap.innerHTML =
    (poster ? '<img src="' + poster + '" alt="">' : '') +
    '<span style="position:absolute;bottom:4px;left:4px;font-size:9px;font-weight:700;letter-spacing:.05em;color:#fff;background:rgba(0,0,0,.65);padding:2px 6px;border-radius:3px">VIDEO</span>';
  wrap.appendChild(_makeRemoveButton(function () {
    modal.querySelectorAll('input[data-video-asset="1"]').forEach(function (n) { n.remove(); });
    wrap.remove();
    _updateMediaGrid();
    _syncAltBtn();
  }));
  _syncAltBtn();
}

function _updateMediaGrid() {
  var prev = document.getElementById('media-previews');
  if (!prev) return;
  var count = prev.children.length;
  if (count === 0) { prev.removeAttribute('data-count'); }
  else { prev.dataset.count = String(Math.min(count, 4)); }
}
function _syncAltBtn() {
  var prev = document.getElementById('media-previews');
  var btn  = document.getElementById('compose-alttext-btn');
  if (!btn) return;
  var hasMedia = prev && prev.children.length > 0;
  btn.style.display = hasMedia ? '' : 'none';
}

// _showDuplicateVideoWarning — shown in compose when uploaded video already exists.
// The warning persists so the user knows they're posting a reference, not the original.
function _showDuplicateVideoWarning(handle, postId, modal) {
  var existing = (modal || document).querySelector('.compose-dup-warning');
  if (existing) existing.remove();
  var warn = document.createElement('div');
  warn.className = 'compose-dup-warning';
  var postHref = postId ? '/post/' + postId : '/' + handle;
  warn.innerHTML =
    '<svg fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24" style="width:14px;height:14px;flex-shrink:0">' +
      '<path d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"/>' +
      '<line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>' +
    '</svg>' +
    '<span>This is already on F33D3R — ' +
    '<a href="' + postHref + '" target="_blank" rel="noopener" ' +
       'style="color:var(--accent);font-weight:700;text-decoration:none" onclick="event.stopPropagation()">@' + (handle || 'another creator') + '</a>' +
    ' will retain the credit.</span>';
  var card = modal ? modal.querySelector('.compose-card') : document.querySelector('.compose-card');
  var toolbar = card ? card.querySelector('.compose-toolbar') : null;
  if (toolbar) {
    card.insertBefore(warn, toolbar);
  } else if (card) {
    card.appendChild(warn);
  }
}

function _showSelfDuplicateWarning(modal) {
  var existing = (modal || document).querySelector('.compose-dup-warning');
  if (existing) existing.remove();
  var warn = document.createElement('div');
  warn.className = 'compose-dup-warning compose-dup-warning--self';
  warn.innerHTML =
    '<svg fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24" style="width:14px;height:14px;flex-shrink:0">' +
      '<circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/>' +
    '</svg>' +
    '<span>You already have this on F33D3R. We\'ll use your existing copy so it stays one post.</span>';
  var card = modal ? modal.querySelector('.compose-card') : document.querySelector('.compose-card');
  var toolbar = card ? card.querySelector('.compose-toolbar') : null;
  if (toolbar) card.insertBefore(warn, toolbar);
  else if (card) card.appendChild(warn);
}

function _makeRemoveButton(onClick) {
  var rm = document.createElement('button');
  rm.type = 'button'; rm.textContent = '\xD7';
  rm.style.cssText = 'position:absolute;top:3px;right:3px;width:20px;height:20px;border-radius:50%;background:rgba(0,0,0,.65);color:#fff;font-size:14px;line-height:20px;text-align:center;border:none;cursor:pointer;padding:0';
  rm.onclick = onClick;
  return rm;
}

window.clearMediaPreviews = function() {
  var prev = document.getElementById('media-previews');
  if (prev) { prev.innerHTML = ''; prev.removeAttribute('data-count'); }
  document.querySelectorAll('#compose-modal [name="media_urls"]').forEach(function(el) { el.remove(); });
  document.querySelectorAll('#compose-modal input[data-video-asset="1"]').forEach(function(el) { el.remove(); });
};

// ── Thread compose ────────────────────────────────────────────────────────────
var _threadMode = false;

window.toggleThreadCompose = function() {
  _threadMode = !_threadMode;
  var segs  = document.getElementById('thread-segments');
  var addBtn = document.getElementById('thread-add-btn');
  var threadBtn = document.getElementById('compose-thread-btn');
  if (!segs || !addBtn || !threadBtn) return;
  segs.style.display  = _threadMode ? '' : 'none';
  addBtn.style.display = _threadMode ? '' : 'none';
  threadBtn.style.color = _threadMode ? 'var(--accent)' : '';
  if (_threadMode && segs.children.length === 0) {
    _addThreadSegment();
  }
};

function _addThreadSegment() {
  var segs = document.getElementById('thread-segments');
  if (!segs) return;
  var idx = segs.children.length + 1;
  var wrap = document.createElement('div');
  wrap.style.cssText = 'display:flex;gap:10px;align-items:flex-start;margin-top:8px;padding-left:0';
  wrap.innerHTML =
    '<div style="display:flex;flex-direction:column;align-items:center;width:40px;flex-shrink:0">' +
      '<div style="width:2px;height:16px;background:var(--border-soft);margin-bottom:4px"></div>' +
    '</div>' +
    '<textarea class="compose-textarea thread-seg-ta" rows="2" maxlength="2000" ' +
      'placeholder="Continue your thread…" ' +
      'style="flex:1;resize:vertical;min-height:56px" ' +
      'data-thread-seg="' + idx + '"></textarea>';
  segs.appendChild(wrap);
  wrap.querySelector('textarea').focus();
}

function _clearThreadSegments() {
  _threadMode = false;
  var segs   = document.getElementById('thread-segments');
  var addBtn = document.getElementById('thread-add-btn');
  var threadBtn = document.getElementById('compose-thread-btn');
  if (segs)  { segs.innerHTML = ''; segs.style.display = 'none'; }
  if (addBtn) addBtn.style.display = 'none';
  if (threadBtn) threadBtn.style.color = '';
}

window.submitThreadCompose = async function() {
  var ta   = document.getElementById('compose-textarea');
  var btn  = document.getElementById('compose-post-btn');
  if (!ta || !ta.value.trim()) return;
  var segments = [ta.value.trim()];
  document.querySelectorAll('.thread-seg-ta').forEach(function(t) {
    var v = t.value.trim();
    if (v) segments.push(v);
  });
  if (segments.length < 2) {
    toast('Add at least one more segment to post a thread', 'error');
    return;
  }
  if (!window.Malkuth || !window._malkuthReady) {
    toast('Signing key unavailable — please reload the page.', 'error');
    return;
  }
  var _mk = await window._malkuthReady;
  if (!_mk || !_mk.hasKey) {
    toast('Signing key unavailable — please reload the page.', 'error');
    return;
  }
  var gating = (document.getElementById('compose-gating') || {}).value || 'everyone';
  btn.disabled = true; btn.textContent = 'Posting…';
  try {
    var prevCID = null;
    for (var i = 0; i < segments.length; i++) {
      var opts = { comment_gating: gating };
      var env = await window.Malkuth.buildSignedWork('post', segments[i], 'thread_post', [], prevCID, opts);
      var res = await fetch('/events', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'HX-Request': 'true' },
        body: JSON.stringify(env),
      });
      if (!res.ok) {
        toast(await res.text() || 'Thread post failed', 'error');
        btn.disabled = false; btn.textContent = 'Post';
        return;
      }
      var data = await res.json();
      prevCID = data.cid;
    }
    toast('Thread posted');
    ta.value = '';
    updateCharCount();
    clearMediaPreviews();
    _clearThreadSegments();
    closeCompose();
    _refreshActiveFeed();
  } catch(e) { toast('Thread post failed: ' + e.message, 'error'); }
  btn.disabled = false; btn.textContent = 'Post';
};

// ── User menu ─────────────────────────────────────────────────────────────────
window.toggleUserMenu = function() {
  var p = document.getElementById('user-menu-popup');
  if (p) p.classList.toggle('hidden');
};
document.addEventListener('click', function(e) {
  var wrap  = document.getElementById('sidebar-user-wrap');
  var popup = document.getElementById('user-menu-popup');
  if (wrap && popup && !wrap.contains(e.target)) popup.classList.add('hidden');
});

// ── Mobile drawer ─────────────────────────────────────────────────────────────
window.toggleMobileMore = function() {
  var overlay = document.getElementById('mob-more-overlay');
  var drawer  = document.getElementById('mob-more-drawer');
  if (!overlay || !drawer) return;
  if (drawer.style.display !== 'none') { closeMobileMore(); }
  else { overlay.style.display = ''; drawer.style.display = ''; }
};
window.closeMobileMore = function() {
  var overlay = document.getElementById('mob-more-overlay');
  var drawer  = document.getElementById('mob-more-drawer');
  if (overlay) overlay.style.display = 'none';
  if (drawer)  drawer.style.display  = 'none';
};

// ── Poll compose ──────────────────────────────────────────────────────────────
var _pollMode = false;

window.togglePollCompose = function() {
  _pollMode = !_pollMode;
  var panel = document.getElementById('poll-compose-panel');
  if (panel) {
    panel.style.display = _pollMode ? 'block' : 'none';
    if (_pollMode) {
      var card = document.querySelector('.compose-card');
      if (card && !card.contains(panel)) card.appendChild(panel);
    }
  }
  var btn = document.getElementById('compose-poll-btn');
  if (btn) btn.classList.toggle('active', _pollMode);
};
window.addPollOption = function() {
  var list = document.getElementById('poll-options-list');
  if (!list || list.children.length >= 4) return;
  var inp = document.createElement('input');
  inp.type = 'text'; inp.className = 'poll-option-input';
  inp.placeholder = 'Option ' + (list.children.length + 1); inp.maxLength = 80;
  inp.style.cssText = "width:100%;background:rgba(255,255,255,.04);border:1px solid var(--border);border-radius:var(--radius-sm);padding:8px 12px;color:var(--text-primary);font-size:14px;font-family:'DM Sans',sans-serif;outline:none";
  list.appendChild(inp);
  if (list.children.length >= 4) { var ab = document.getElementById('poll-add-btn'); if (ab) ab.style.display = 'none'; }
};


// ── DM banner (iOS-style top slide-down, high-priority DM only) ──────────────
var _dmBannerTimer = null;
window.showDmBanner = function(data, avatarUrl) {
  var banner = document.getElementById('dm-banner');
  if (!banner) return;
  // data may be "handle: preview" or just a preview
  var from = 'Message';
  var text = typeof data === 'string' ? data : 'New message';
  var colonIdx = text.indexOf(': ');
  if (colonIdx > 0) {
    from = text.slice(0, colonIdx);
    text = text.slice(colonIdx + 2);
  }
  var fromEl = document.getElementById('dm-banner-from');
  var textEl = document.getElementById('dm-banner-text');
  var avEl   = document.getElementById('dm-banner-av');
  if (fromEl) fromEl.textContent = from;
  if (textEl) textEl.textContent = text || 'New message';
  if (avEl) {
    if (avatarUrl) {
      avEl.innerHTML = '<img src="' + avatarUrl + '" alt="">';
    } else {
      avEl.textContent = from.replace(/^@/,'')[0].toUpperCase();
    }
  }
  banner.style.display = 'flex';
  requestAnimationFrame(function() {
    requestAnimationFrame(function() {
      banner.style.transform = 'translateX(-50%) translateY(0)';
    });
  });
  if (_dmBannerTimer) clearTimeout(_dmBannerTimer);
  _dmBannerTimer = setTimeout(function() {
    banner.style.transform = 'translateX(-50%) translateY(-120%)';
    setTimeout(function() { banner.style.display = 'none'; }, 320);
  }, 5000);
};

// ── Post timestamps: format data-ts Unix seconds to local 24h time ───────────
function formatPostTimes(root) {
  (root || document).querySelectorAll('time.post-time[data-ts]').forEach(function(el) {
    var ts = parseInt(el.dataset.ts, 10);
    if (!ts) return;
    var d = new Date(ts * 1000);
    var now = new Date();
    var diffMs = now - d;
    var hhmm = d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', hour12:false});
    var label;
    if (diffMs < 60000) {
      label = 'now';
    } else if (diffMs < 3600000) {
      label = Math.floor(diffMs / 60000) + 'm';
    } else if (diffMs < 86400000) {
      label = hhmm;
    } else if (diffMs < 7 * 86400000) {
      var day = d.toLocaleDateString([], {weekday:'short'});
      label = day + ' ' + hhmm;
    } else {
      var mon = d.toLocaleDateString([], {month:'short', day:'numeric'});
      label = mon + ' ' + hhmm;
    }
    el.textContent = label;
    el.title = d.toLocaleString([], {dateStyle:'medium', timeStyle:'short', hour12:false});
  });
}
document.addEventListener('DOMContentLoaded', function() { formatPostTimes(); hydrateVoiceBars(); });
document.addEventListener('htmx:afterSwap', function(e) { formatPostTimes(e.detail.target); hydrateVoiceBars(e.detail.target); });

// formatVisionRings is gone. It drew an expiry countdown arc in the browser from
// a data-expires timestamp — the browser deciding what a surface looks like from
// state it was handed. Ring state is now one SQL answer on the server
// (db.GetVisionRingsForViewer) and arrives as a class on the vision_ring Facet,
// and expiry is enforced in SQL on every read rather than drawn as an arc.

// ── Media carousel ───────────────────────────────────────────────────────────
function mediaCarouselGoTo(carousel, idx) {
  if (!carousel) return;
  var slides  = carousel.querySelectorAll('.carousel-slide');
  var dots    = carousel.querySelectorAll('.carousel-dot');
  var counter = carousel.querySelector('.carousel-counter');
  var cur = parseInt(carousel.dataset.idx || '0', 10);
  var n   = slides.length;
  idx = ((idx % n) + n) % n;
  if (cur === idx) return;
  slides[cur].classList.remove('active');
  slides[idx].classList.add('active');
  if (dots[cur]) dots[cur].classList.remove('active');
  if (dots[idx]) dots[idx].classList.add('active');
  if (counter) counter.textContent = (idx + 1) + '/' + n;
  carousel.dataset.idx = idx;
}
function mediaCarouselStep(btn, delta) {
  var carousel = btn.closest('.media-carousel');
  if (!carousel) return;
  mediaCarouselGoTo(carousel, parseInt(carousel.dataset.idx || '0', 10) + delta);
}
(function() {
  function initTouch(carousel) {
    if (carousel._f33dTouch) return;
    carousel._f33dTouch = true;
    var sx = 0, sy = 0;
    carousel.addEventListener('touchstart', function(e) { sx = e.touches[0].clientX; sy = e.touches[0].clientY; }, {passive:true});
    carousel.addEventListener('touchend', function(e) {
      var dx = e.changedTouches[0].clientX - sx;
      var dy = e.changedTouches[0].clientY - sy;
      if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > 35)
        mediaCarouselGoTo(carousel, parseInt(carousel.dataset.idx || '0', 10) + (dx < 0 ? 1 : -1));
    }, {passive:true});
  }
  function scan(root) { (root || document).querySelectorAll('.media-carousel').forEach(initTouch); }
  document.addEventListener('DOMContentLoaded', function() { scan(); });
  document.addEventListener('htmx:afterSwap', function(e) { scan(e.detail && e.detail.target); });
})();

// ── Repost sheet — Facet(repost_sheet): in-stack, class-toggle only ──────────
// Sheet is pre-rendered in _repost_sheet.html inside .repost-btn-wrap (position:relative).
// No DOM creation, no getBoundingClientRect, no style.position mutation on post-card.
window.toggleRepostSheet = function(postId) {
  var wrap = document.getElementById('rs-wrap-' + postId);
  if (!wrap) return;
  var isOpen = wrap.classList.contains('repost-btn-wrap--open');
  // Close all open sheets first
  document.querySelectorAll('.repost-btn-wrap--open').forEach(function(w) {
    w.classList.remove('repost-btn-wrap--open');
  });
  if (!isOpen) {
    wrap.classList.add('repost-btn-wrap--open');
    setTimeout(function() { document.addEventListener('click', _closeRepostSheets); }, 0);
  }
};
window.closeRepostSheet = function(postId) {
  var wrap = document.getElementById('rs-wrap-' + postId);
  if (wrap) wrap.classList.remove('repost-btn-wrap--open');
};
function _closeRepostSheets(e) {
  if (!e.target.closest('.repost-btn-wrap')) {
    document.querySelectorAll('.repost-btn-wrap--open').forEach(function(w) {
      w.classList.remove('repost-btn-wrap--open');
    });
    document.removeEventListener('click', _closeRepostSheets);
  }
}
// Legacy doRepost (POST /feed/item/repost) removed — reposts are works-native
// via toggleWorkReaction(...,'repost',...) → POST /events work_repost.
window.openRepostSheet = function(btn, postId) { window.toggleRepostSheet(postId); };

// Facet(quote_compose) — opens compose pre-loaded with quoted work ID + preview.
// The server recalls the whole work by id and returns the playable quoted_work
// embed; we never assemble the work from passed-in strings.
window.openQuoteCompose = function(postId) {
  var modal = document.getElementById('compose-modal');
  if (!modal) return;
  // Stash quoted_post_id
  var qi = modal.querySelector('input[name="quoted_post_id"]');
  if (!qi) { qi = document.createElement('input'); qi.type='hidden'; qi.name='quoted_post_id'; modal.appendChild(qi); }
  qi.value = postId;
  // Update header
  var hdr = modal.querySelector('.compose-header span');
  if (hdr) hdr.textContent = 'Quote post';
  // Reset any existing link preview state — quote takes over the preview slot
  _composeLinkPreviewDismissed = false;
  _composeLinkPreviewURL = '';
  // Fetch the server-rendered quoted_work embed and inject into the preview slot
  var slot = document.getElementById('compose-preview-slot');
  if (slot) {
    slot.innerHTML = '<div class="cps-loading">Loading preview…</div>';
    fetch('/facets/quoted_post?post_id=' + encodeURIComponent(postId))
      .then(function(r) { return r.ok ? r.text() : null; })
      .then(function(html) {
        if (!slot) return;
        if (!html) { slot.innerHTML = ''; return; }
        slot.innerHTML = '<div class="cps-wrap cps-wrap--quote">' + html + '</div>';
        // Raw fetch insert (not an HTMX swap) — wire any embedded HLS video so
        // it gets a player and is playable in the preview.
        if (window.F33D3R_Video) { try { F33D3R_Video.attachAll(slot); } catch(e){} }
      })
      .catch(function() { if (slot) slot.innerHTML = ''; });
  }
  openCompose();
};

// Facet(work_repost_sheet) — quote entry point for works; delegates to openQuoteCompose
window.openWorkQuoteCompose = function(workId) {
  openQuoteCompose(workId);
};

// Listen for replyPosted HX-Trigger to update comment count on reply button
document.addEventListener('htmx:afterRequest', function(e) {
  var hdr = (e.detail.xhr && e.detail.xhr.getResponseHeader('HX-Trigger')) || '';
  if (!hdr || hdr.indexOf('replyPosted') === -1) return;
  try {
    var d = JSON.parse(hdr).replyPosted;
    var el = document.getElementById('reply-count-' + d.parentId);
    if (el) el.textContent = String(d.count);
  } catch(err) {}
});

// Generic server-driven toast: any handler can confirm an action by returning
// HX-Trigger: {"f33Toast":"message"} (e.g. pin/unpin). Reusable feedback pipe.
document.addEventListener('htmx:afterRequest', function(e) {
  var hdr = (e.detail.xhr && e.detail.xhr.getResponseHeader('HX-Trigger')) || '';
  if (!hdr || hdr.indexOf('f33Toast') === -1) return;
  try {
    var msg = JSON.parse(hdr).f33Toast;
    if (msg && typeof toast === 'function') toast(msg);
  } catch(err) {}
});

// ── Toast ─────────────────────────────────────────────────────────────────────
window.toast = function(msg, type) {
  type = type || 'success';
  var t = document.createElement('div');
  t.className = 'toast toast-' + type;
  t.textContent = msg;
  var container = document.getElementById('toast-container');
  if (container) container.appendChild(t);
  setTimeout(function() { t.classList.add('toast-show'); }, 10);
  setTimeout(function() { t.classList.remove('toast-show'); setTimeout(function() { t.remove(); }, 300); }, 3000);
};

// ── NEXUS PersonaSwitcher ─────────────────────────────────────────────────────
// Opens/closes the persona sheet. Called from persona-switcher.html onclick attrs.
window.PersonaSwitcher = (function() {
  function open() {
    var sheet = document.getElementById('persona-switcher-sheet');
    if (sheet) {
      sheet.classList.add('open');
      sheet.setAttribute('aria-hidden', 'false');
      document.addEventListener('keydown', _closeOnEsc);
    }
  }
  function close() {
    var sheet = document.getElementById('persona-switcher-sheet');
    if (sheet) {
      sheet.classList.remove('open');
      sheet.setAttribute('aria-hidden', 'true');
      document.removeEventListener('keydown', _closeOnEsc);
    }
  }
  function _closeOnEsc(e) {
    if (e.key === 'Escape') close();
  }
  return { open: open, close: close };
})();

// Facet(sensitive_gate) — dismiss the gate and reveal gated media for a post.
// No server round-trip; purely client-side reveal.
window.revealSensitive = function(postId) {
  var gate = document.getElementById('sg-' + postId);
  var body = document.getElementById('sg-body-' + postId);
  if (gate) gate.remove();
  if (body) body.style.display = '';
};

// toggleDarkMode and _syncModeIcons are defined in base.html inline script
// so they are available before this file loads. No duplicate needed here.

// ── Theme live preview — shared by settings page and edit_profile_modal ──────
// Applies the selected theme to the page body immediately so the user sees the
// colour change before saving the form. Exposed as window.applyThemePreview so
// it is available on every page (the edit profile modal is injected via HTMX
// into #overlay-slot on any page, not just /settings).
window.applyThemePreview = function(themeId) {
  document.body.dataset.theme = themeId;
};

// ── Facet(tip_modal) — presets only; the modal itself is a server Facet ──
window.setTipAmount = function(amount) {
  var input = document.getElementById('tip-amount-input');
  if (input) { input.value = amount; input.focus(); }
};
window.tipCopyAddr = function(btn) {
  var addr = btn.dataset.addr;
  if (!addr) return;
  navigator.clipboard.writeText(addr).then(function() {
    var orig = btn.innerHTML;
    btn.textContent = 'Copied!';
    btn.disabled = true;
    setTimeout(function() { btn.innerHTML = orig; btn.disabled = false; }, 2000);
  }).catch(function() {
    if (typeof toast === 'function') toast('Copy failed — address: ' + addr.slice(0, 16) + '…', 'error');
  });
};
// ── ShareSheet facet actions (browser hardware projection; server owns the link) ──
// Copy the canonical work link to the clipboard, with inline button feedback.
window.shareCopy = function(btn) {
  var input = document.getElementById('share-sheet-url');
  var url = input ? input.value : '';
  if (!url) return;
  navigator.clipboard.writeText(url).then(function() {
    var orig = btn.textContent;
    btn.textContent = 'Copied!';
    btn.disabled = true;
    setTimeout(function() { btn.textContent = orig; btn.disabled = false; }, 2000);
  }).catch(function() {
    if (typeof toast === 'function') toast('Copy failed', 'error');
  });
};
// Invoke the native share sheet (Instagram, Snapchat, …) via the Web Share API.
// Degrades gracefully to clipboard copy where navigator.share is unavailable.
window.shareNative = function(btn) {
  var url  = btn.dataset.shareUrl;
  var text = btn.dataset.shareText || '';
  if (!url) return;
  if (navigator.share) {
    navigator.share({ title: 'f33d3r', text: text, url: url }).catch(function() {});
  } else {
    window.shareCopy(btn);
    if (typeof toast === 'function') toast('Link copied');
  }
};
/* ─── View-time tracking — Gap 1 closure ───────────────────────────────── */
// Tracks how long each post card is visible in the viewport (IntersectionObserver).
// For video posts, records actual currentTime instead of wall-clock dwell.
// Flushes on scroll-out and on page hide. Fires POST /events view_time.
(function() {
  if (typeof IntersectionObserver === 'undefined') return;

  // Map of contentId → {enterTime, videoEl}
  var _watching = {};

  function _getSessionAndSurface() {
    var si = document.querySelector('[name="session_id"]');
    var su = document.querySelector('[name="surface"]');
    return {
      session_id: si ? si.value : '',
      surface:    su ? su.value : ''
    };
  }

  function _flushContent(contentId, info) {
    if (!info || !info.enterTime) return;
    var secs;
    if (info.videoEl && !info.videoEl.paused && info.videoEl.currentTime > 0) {
      secs = info.videoEl.currentTime;
    } else {
      secs = (Date.now() - info.enterTime) / 1000;
    }
    // Ignore blinks under 1 second or implausibly long (tab was hidden)
    if (secs < 1 || secs > 3600) return;
    var ctx = _getSessionAndSurface();
    var body = new URLSearchParams();
    body.set('event_type', 'view_time');
    body.set('post_id',    contentId);
    body.set('seconds',    secs.toFixed(2));
    body.set('session_id', ctx.session_id);
    body.set('surface',    ctx.surface);
    fetch('/events', { method: 'POST', body: body, keepalive: true });
  }

  function _attachObserver(root) {
    var observer = new IntersectionObserver(function(entries) {
      entries.forEach(function(entry) {
        var el = entry.target;
        var cid = el.dataset.contentId;
        if (!cid) return;
        if (entry.isIntersecting) {
          var videoEl = el.querySelector('video');
          _watching[cid] = { enterTime: Date.now(), videoEl: videoEl || null };
        } else {
          if (_watching[cid]) {
            _flushContent(cid, _watching[cid]);
            delete _watching[cid];
          }
        }
      });
    }, { threshold: 0.5 }); // 50% visible triggers enter/exit

    root.querySelectorAll('[data-content-id]').forEach(function(el) {
      observer.observe(el);
    });
    return observer;
  }

  // Initial attach
  var _rootObserver = _attachObserver(document);

  // Re-attach after HTMX swaps inject new post cards
  document.addEventListener('htmx:afterSwap', function(e) {
    if (e.detail && e.detail.elt) {
      e.detail.elt.querySelectorAll('[data-content-id]').forEach(function(el) {
        _rootObserver.observe(el);
      });
    }
  });

  // Flush all in-progress on page hide (tab switch, close, navigate)
  document.addEventListener('visibilitychange', function() {
    if (document.visibilityState === 'hidden') {
      Object.keys(_watching).forEach(function(cid) {
        _flushContent(cid, _watching[cid]);
      });
      _watching = {};
    }
  });
})();

// ── Compose: Drafts (IndexedDB) + Quote Picker ───────────────────────────────

var ComposeDrafts = (function() {
  var DB_NAME = 'f33d3r_compose';
  var STORE   = 'drafts';
  var _db     = null;

  function _openDB() {
    if (_db) return Promise.resolve(_db);
    return new Promise(function(resolve, reject) {
      var req = indexedDB.open(DB_NAME, 1);
      req.onupgradeneeded = function(e) {
        e.target.result.createObjectStore(STORE, { keyPath: 'id' });
      };
      req.onsuccess = function(e) { _db = e.target.result; resolve(_db); };
      req.onerror   = function()  { reject(new Error('IDB open failed')); };
    });
  }

  function _genId() {
    return 'draft_' + Date.now() + '_' + Math.random().toString(36).slice(2, 7);
  }

  function save(draft) {
    return _openDB().then(function(db) {
      if (!draft.id) draft.id = _genId();
      draft.savedAt = Date.now();
      return new Promise(function(resolve, reject) {
        var tx  = db.transaction(STORE, 'readwrite');
        var req = tx.objectStore(STORE).put(draft);
        req.onsuccess = function() { updateBtn(); resolve(draft.id); };
        req.onerror   = reject;
      });
    });
  }

  function list() {
    return _openDB().then(function(db) {
      return new Promise(function(resolve, reject) {
        var req = db.transaction(STORE, 'readonly').objectStore(STORE).getAll();
        req.onsuccess = function(e) { resolve(e.target.result || []); };
        req.onerror   = reject;
      });
    });
  }

  function remove(id) {
    return _openDB().then(function(db) {
      return new Promise(function(resolve, reject) {
        var tx = db.transaction(STORE, 'readwrite');
        tx.objectStore(STORE).delete(id);
        tx.oncomplete = function() { updateBtn(); resolve(); };
        tx.onerror    = reject;
      });
    });
  }

  function count() {
    return _openDB().then(function(db) {
      return new Promise(function(resolve, reject) {
        var req = db.transaction(STORE, 'readonly').objectStore(STORE).count();
        req.onsuccess = function(e) { resolve(e.target.result || 0); };
        req.onerror   = reject;
      });
    });
  }

  function updateBtn() {
    count().then(function(n) {
      var btn = document.getElementById('compose-drafts-btn');
      if (!btn) return;
      btn.style.display = n > 0 ? 'inline-flex' : 'none';
      btn.textContent   = n === 1 ? 'Drafts' : 'Drafts (' + n + ')';
    }).catch(function() {});
  }

  return { save: save, list: list, remove: remove, count: count, updateBtn: updateBtn };
})();

// Auto-save state
var _draftAutoSaveTimer = null;
var _currentDraftId     = null;
var _draftExplicitlySaved = false;

// Wire up textarea auto-save and close-button draft prompt after DOMContentLoaded
document.addEventListener('DOMContentLoaded', function() {
  ComposeDrafts.updateBtn();

  var ta = document.getElementById('compose-textarea');
  if (ta) {
    ta.addEventListener('input', function() {
      clearTimeout(_draftAutoSaveTimer);
      if (ta.value.trim().length < 10) return;
      _draftAutoSaveTimer = setTimeout(function() {
        ComposeDrafts.save({ id: _currentDraftId, body: ta.value })
          .then(function(id) { _currentDraftId = id; })
          .catch(function() {});
      }, 5000);
    });
  }

  // Close button: intercept in capture phase so we fire before the existing listener
  var closeBtn = document.getElementById('compose-close-btn');
  if (closeBtn) {
    closeBtn.addEventListener('click', function(e) {
      var textarea = document.getElementById('compose-textarea');
      if (textarea && textarea.value.trim().length > 10) {
        e.stopImmediatePropagation(); // prevent the existing closeCompose listener
        _showComposeDraftPrompt();
      }
      // If no content, existing listener fires normally and closes compose
    }, true /* capture phase */);
  }
});

// Refresh drafts button + reset flag each time compose opens
(function() {
  var _prevOpen = window.openCompose;
  window.openCompose = function() {
    _draftExplicitlySaved = false;
    ComposeDrafts.updateBtn();
    _prevOpen && _prevOpen.apply(this, arguments);
  };
})();

// Wrap closeCompose to clean up our panels + discard ephemeral auto-saved draft
(function() {
  var _prevClose = window.closeCompose;
  window.closeCompose = function() {
    clearTimeout(_draftAutoSaveTimer);
    // Discard the ephemeral auto-saved draft if the user didn't explicitly save
    if (_currentDraftId && !_draftExplicitlySaved) {
      ComposeDrafts.remove(_currentDraftId).catch(function() {});
    }
    _currentDraftId       = null;
    _draftExplicitlySaved = false;
    // Remove compose-specific sub-panels
    ['compose-quote-picker', 'compose-drafts-sheet', 'compose-draft-prompt'].forEach(function(id) {
      var el = document.getElementById(id);
      if (el) el.remove();
    });
    _prevClose && _prevClose.apply(this, arguments);
  };
})();

function _showComposeDraftPrompt() {
  if (document.getElementById('compose-draft-prompt')) return;
  var card = document.getElementById('compose-card');
  if (!card) return;
  var bar = document.createElement('div');
  bar.id        = 'compose-draft-prompt';
  bar.className = 'compose-draft-prompt';
  bar.innerHTML =
    '<span class="compose-draft-prompt-text">Save as draft?</span>' +
    '<button type="button" class="compose-draft-save-btn" onclick="_composeSaveDraft()">Save</button>' +
    '<button type="button" class="compose-draft-discard-btn" onclick="_composeDiscardDraft()">Discard</button>';
  card.appendChild(bar);
}

window._composeSaveDraft = function() {
  var ta   = document.getElementById('compose-textarea');
  var body = ta ? ta.value : '';
  _draftExplicitlySaved = true;
  ComposeDrafts.save({ id: _currentDraftId, body: body })
    .then(function() {
      var bar = document.getElementById('compose-draft-prompt');
      if (bar) bar.remove();
      closeCompose();
    })
    .catch(function() { closeCompose(); });
};

window._composeDiscardDraft = function() {
  var bar = document.getElementById('compose-draft-prompt');
  if (bar) bar.remove();
  closeCompose();
};

window.openComposeDrafts = function() {
  var existing = document.getElementById('compose-drafts-sheet');
  if (existing) { existing.remove(); return; }
  var card = document.getElementById('compose-card');
  if (!card) return;

  ComposeDrafts.list().then(function(drafts) {
    var sheet = document.createElement('div');
    sheet.id        = 'compose-drafts-sheet';
    sheet.className = 'compose-drafts-sheet';

    var header =
      '<div class="compose-drafts-header">' +
        '<span class="compose-drafts-title">Drafts</span>' +
        '<button type="button" class="compose-drafts-close-btn" ' +
          'onclick="document.getElementById(\'compose-drafts-sheet\').remove()">Done</button>' +
      '</div>';

    if (!drafts || !drafts.length) {
      sheet.innerHTML = header + '<p class="compose-drafts-empty">No saved drafts</p>';
    } else {
      var sorted = drafts.slice().sort(function(a, b) { return (b.savedAt || 0) - (a.savedAt || 0); });
      var rows = sorted.map(function(d) {
        var preview = (d.body || '').slice(0, 80).replace(/</g, '&lt;').replace(/>/g, '&gt;');
        var ts = d.savedAt
          ? new Date(d.savedAt).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
          : '';
        var did = d.id.replace(/'/g, "\\'");
        return '<button type="button" class="compose-draft-row" onclick="loadComposeDraft(\'' + did + '\')">' +
          '<span class="compose-draft-preview">' + preview + '</span>' +
          '<span class="compose-draft-time">' + ts + '</span>' +
          '<button type="button" class="compose-draft-delete-btn" aria-label="Delete draft" ' +
            'onclick="event.stopPropagation();deleteComposeDraft(\'' + did + '\')">×</button>' +
        '</button>';
      }).join('');
      sheet.innerHTML = header + '<div class="compose-drafts-list">' + rows + '</div>';
    }
    card.appendChild(sheet);
  }).catch(function() {});
};

window.loadComposeDraft = function(id) {
  ComposeDrafts.list().then(function(drafts) {
    var draft = drafts.find(function(d) { return d.id === id; });
    if (!draft) return;
    var ta = document.getElementById('compose-textarea');
    if (ta) {
      ta.value = draft.body || '';
      ta.dispatchEvent(new Event('input'));
    }
    _currentDraftId = id;
    var sheet = document.getElementById('compose-drafts-sheet');
    if (sheet) sheet.remove();
  }).catch(function() {});
};

window.deleteComposeDraft = function(id) {
  ComposeDrafts.remove(id).then(function() {
    var sheet = document.getElementById('compose-drafts-sheet');
    if (sheet) { sheet.remove(); window.openComposeDrafts(); }
  }).catch(function() {});
};

// ── Compose: Quote Picker ─────────────────────────────────────────────────────

window.openQuotePicker = function() {
  var existing = document.getElementById('compose-quote-picker');
  if (existing) { existing.remove(); document.getElementById('compose-quote-pick-btn').classList.remove('active'); return; }
  var card = document.getElementById('compose-card');
  if (!card) return;

  var panel = document.createElement('div');
  panel.id        = 'compose-quote-picker';
  panel.className = 'compose-quote-picker';
  panel.innerHTML =
    '<div class="compose-quote-picker-header">' +
      '<button type="button" class="compose-quote-picker-cancel" onclick="closeQuotePicker()">Cancel</button>' +
      '<span class="compose-quote-picker-title">Quote a post</span>' +
    '</div>' +
    '<div class="compose-quote-picker-tabs" role="tablist">' +
      '<button type="button" class="compose-quote-tab compose-quote-tab--active" role="tab" ' +
        'onclick="switchQuoteTab(this,\'/partials/compose/quote-picker/bookmarks\')">Bookmarks</button>' +
      '<button type="button" class="compose-quote-tab" role="tab" ' +
        'onclick="switchQuoteTab(this,\'/partials/compose/quote-picker/likes\')">Likes</button>' +
      '<button type="button" class="compose-quote-tab" role="tab" ' +
        'onclick="switchQuoteTab(this,\'/partials/compose/quote-picker/yours\')">Your posts</button>' +
    '</div>' +
    '<div id="compose-quote-results" class="compose-quote-results"></div>';
  card.appendChild(panel);

  var btn = document.getElementById('compose-quote-pick-btn');
  if (btn) btn.classList.add('active');

  _loadQuoteTab('/partials/compose/quote-picker/bookmarks');
};

window.closeQuotePicker = function() {
  var panel = document.getElementById('compose-quote-picker');
  if (panel) panel.remove();
  var btn = document.getElementById('compose-quote-pick-btn');
  if (btn) btn.classList.remove('active');
};

window.switchQuoteTab = function(tabBtn, url) {
  document.querySelectorAll('.compose-quote-tab').forEach(function(t) {
    t.classList.remove('compose-quote-tab--active');
  });
  tabBtn.classList.add('compose-quote-tab--active');
  _loadQuoteTab(url);
};

function _loadQuoteTab(url) {
  var results = document.getElementById('compose-quote-results');
  if (!results) return;
  results.innerHTML = '<p class="compose-quote-loading">Loading…</p>';
  fetch(url, { headers: { 'HX-Request': 'true' } })
    .then(function(r) { return r.text(); })
    .then(function(html) { results.innerHTML = html; })
    .catch(function() { results.innerHTML = '<p class="compose-quote-empty">Failed to load</p>'; });
}

window.selectQuote = function(btn) {
  closeQuotePicker();
  openQuoteCompose(btn.dataset.postId);
};


/* ─── Edit post — reveal within 60-minute window on HTMX swaps ─── */
function revealEditButtons(root) {
  var EDIT_WINDOW_MS = 60 * 60 * 1000;
  var now = Date.now();
  var scope = root || document;
  scope.querySelectorAll('.post-edit-btn[data-created]').forEach(function(btn) {
    var created = parseInt(btn.dataset.created, 10) * 1000;
    if (now - created < EDIT_WINDOW_MS) {
      btn.style.display = '';
      var remaining = EDIT_WINDOW_MS - (now - created);
      setTimeout(function() { btn.style.display = 'none'; }, remaining);
    }
  });
}
document.addEventListener('DOMContentLoaded', function() { revealEditButtons(); });
document.addEventListener('htmx:afterSwap', function(e) { revealEditButtons(e.detail.target); });

/* ─── Pay-per-view / subscriber-only compose toggle ─── */
window.toggleComposePPV = function() {
  var btn = document.getElementById('compose-ppv-btn');
  var inp = document.getElementById('compose-subscribers-only');
  if (!btn || !inp) return;
  var on = inp.value === '1';
  inp.value = on ? '' : '1';
  btn.classList.toggle('compose-ppv-btn--active', !on);
  btn.title = on ? 'Subscribers only' : 'Subscribers only (on)';
  if (!on) toast('Subscribers only — only your subscribers will see this post');
};

// ── YouTube lazy embed ────────────────────────────────────────────────────────
// Swaps thumbnail+play-button for a youtube-nocookie.com iframe on first click.
// No YouTube JS is loaded until the user explicitly plays.
window.ytLazyPlay = function(wrap) {
  var vid = wrap.dataset.vid;
  if (!vid) return;
  var list = wrap.dataset.list || '';
  var src = 'https://www.youtube-nocookie.com/embed/' + vid + '?autoplay=1&rel=0&modestbranding=1';
  if (list) src += '&list=' + encodeURIComponent(list);
  var iframe = document.createElement('iframe');
  iframe.className = 'yt-embed';
  iframe.src = src;
  iframe.title = 'YouTube video';
  iframe.setAttribute('allow', 'accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture; web-share');
  iframe.setAttribute('allowfullscreen', '');
  wrap.innerHTML = '';
  wrap.appendChild(iframe);
  wrap.classList.remove('yt-embed-wrap--lazy');
};
