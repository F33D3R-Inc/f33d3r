// feed_items.js — post card actions. Loaded once by the Shell (base.html);
// every function here is reached from card markup through onclick, so each is
// published on window. Navigation to a post or work is a Playground navigation.
F33D3R.page('feed_items', function () {
  const _uuidRe = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

  function goToWork(id) {
    if (id && _uuidRe.test(id)) F33D3R.visit('/work/' + id);
  }

  async function toggleWorkReaction(btn, workID, type, isActive) {
    const eventType = isActive ? 'work_un' + type : 'work_' + type;
    const countEl = btn.querySelector('span');
    const svgEl   = btn.querySelector('svg');
    function apply(active) {
      if (active) {
        btn.classList.add('is-on');
        if (type === 'bookmark') btn.classList.add('saved');
        if ((type === 'like' || type === 'bookmark' || type === 'dislike') && svgEl) svgEl.setAttribute('fill', 'currentColor');
        if (countEl && type !== 'bookmark') { var n = parseInt(countEl.textContent || 0) + 1; countEl.textContent = n; countEl.style.display = ''; }
      } else {
        btn.classList.remove('is-on', 'saved');
        if ((type === 'like' || type === 'bookmark' || type === 'dislike') && svgEl) svgEl.setAttribute('fill', 'none');
        if (countEl && type !== 'bookmark') { var n = Math.max(0, parseInt(countEl.textContent || 0) - 1); countEl.textContent = n; if (n === 0) countEl.style.display = 'none'; }
      }
      btn.setAttribute('onclick', `event.stopPropagation();toggleWorkReaction(this,'${workID}','${type}',${active})`);
    }
    apply(!isActive);
    try {
      const fd = new FormData();
      fd.append('event_type', eventType);
      fd.append('work_id', workID);
      const r = await fetch('/events', { method: 'POST', body: fd });
      if (!r.ok) { apply(isActive); if (typeof toast === 'function') toast('Action failed — please try again', 'error'); }
    } catch (e) {
      apply(isActive);
      if (typeof toast === 'function') toast('Action failed — please try again', 'error');
    }
  }

  function goToPost(id, cid, sid, surface, rank) {
    fetch('/api/feedback', {method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({user_id:'', session_id:sid, surface:surface,
        events:[{content_id:cid, event_type:'click', position_at_display:rank+1,
          timestamp:new Date().toISOString(), exploration_slot:false}]})});
    if (id && _uuidRe.test(id)) F33D3R.visit('/post/' + id);
  }

  function expandFocusBody(id) {
    const el = document.getElementById('fbd-' + id);
    const btn = document.getElementById('fbm-' + id);
    if (!el) return;
    el.classList.toggle('expanded');
    if (btn) btn.textContent = el.classList.contains('expanded') ? 'less' : 'more';
  }



  // work_menu_dropdown portals into #work-menu-overlay (Layer 3 wire template slot)
  // so it escapes the post_card stacking context and renders above all feed content.
  function togglePostMenu(btn) {
    var portal = document.getElementById('work-menu-overlay');
    if (!portal) return;
    var workId = btn.dataset.postId;
    if (portal.firstChild && portal.firstChild.dataset.menuFor === workId) {
      closeAllPostMenus();
      return;
    }
    closeAllPostMenus();
    var wrap = btn.closest('.post-menu-wrap');
    var src  = wrap && wrap.querySelector('.post-menu-dropdown');
    if (!src) return;
    var rect  = btn.getBoundingClientRect();
    var clone = src.cloneNode(true);
    clone.dataset.menuFor = workId;
    clone.style.top   = (rect.bottom + 4) + 'px';
    clone.style.right = (window.innerWidth - rect.right) + 'px';
    clone.style.left  = 'auto';
    portal.appendChild(clone);
    if (window.htmx) htmx.process(clone);
  }
  function closeAllPostMenus() {
    var portal = document.getElementById('work-menu-overlay');
    if (portal) portal.innerHTML = '';
  }

  F33D3R.once('feed_items:menu-close', function () {
    document.addEventListener('click', function(e) {
      if (!e.target.closest('.post-menu-wrap') && !e.target.closest('#work-menu-overlay')) {
        closeAllPostMenus();
      }
    });
  });

  // Edit-button reveal (60-minute window) is owned by revealEditButtons() in
  // f33d3r.js, which runs on DOMContentLoaded AND htmx:afterSwap — so streamed-in
  // cards reveal correctly too. No duplicate reveal here.

  function openEditPost(postId, btn) {
    const article = btn.closest('article');
    const bodyEl  = article.querySelector('.post-body') || article.querySelector('.focus-body');
    if (!bodyEl) return;
    const currentText = bodyEl.innerText.trim();
    const wrapper = document.createElement('div');
    wrapper.style.cssText = 'padding:8px 0';
    // The card <article> has an onclick navigation handler (goToPost/goToWork);
    // swallow clicks inside the editor so typing/Save doesn't navigate away.
    wrapper.onclick = function(e) { e.stopPropagation(); };
    wrapper.innerHTML = `
      <textarea id="edit-ta-${postId}" style="width:100%;min-height:80px;background:var(--panel);border:1px solid var(--accent);border-radius:8px;padding:10px;color:var(--text-primary);font-size:14px;font-family:inherit;resize:vertical;outline:none" maxlength="2000">${currentText.replace(/</g,'&lt;').replace(/>/g,'&gt;')}</textarea>
      <div style="display:flex;gap:8px;margin-top:6px;justify-content:flex-end">
        <button onclick="cancelEditPost('${postId}')" style="padding:5px 14px;border-radius:8px;border:1px solid var(--border);background:none;color:var(--text-secondary);font-size:13px;cursor:pointer">Cancel</button>
        <button onclick="saveEditPost('${postId}')" style="padding:5px 14px;border-radius:8px;border:none;background:var(--accent);color:var(--surface);font-size:13px;font-weight:600;cursor:pointer">Save</button>
      </div>`;
    bodyEl._originalHTML = bodyEl.innerHTML;
    bodyEl.innerHTML = '';
    bodyEl.appendChild(wrapper);
    const ta = document.getElementById('edit-ta-' + postId);
    if (ta) { ta.focus(); ta.selectionStart = ta.value.length; }
  }
  function cancelEditPost(postId) {
    const ta = document.getElementById('edit-ta-' + postId);
    if (!ta) return;
    const article = ta.closest('article');
    const bodyEl  = article.querySelector('.post-body') || article.querySelector('.focus-body');
    if (bodyEl && bodyEl._originalHTML) { bodyEl.innerHTML = bodyEl._originalHTML; }
  }
  async function saveEditPost(workId) {
    const ta = document.getElementById('edit-ta-' + workId);
    if (!ta) return;
    const body = ta.value.trim();
    if (!body) return;
    const article = ta.closest('article');
    const bodyEl  = article && (article.querySelector('.post-body') || article.querySelector('.focus-body'));
    const fd = new FormData();
    fd.append('work_id', workId);
    fd.append('body', body);
    try {
      const r = await fetch('/api/work/edit', { method:'POST', body: fd });
      if (r.ok) {
        const html = await r.text();
        // Swap updated body text inline — server returns rendered body fragment.
        if (bodyEl) bodyEl.innerHTML = html;
        toast('Post updated');
      } else {
        const t = await r.text();
        toast(t || 'Edit failed', 'error');
      }
    } catch(e) { toast('Edit failed: ' + e.message, 'error'); }
  }

  function openReplyRestriction(postId, current) {
    const opts = [
      {value:'everyone', label:'Everyone'},
      {value:'followers', label:'Followers only'},
      {value:'verified', label:'Verified users'},
      {value:'none', label:'Nobody'}
    ];
    // Map DB values to display values
    const dbToDisplay = {open:'everyone', followers:'followers', verified:'verified', none:'none'};
    const currentDisplay = dbToDisplay[current] || 'everyone';
    const html = opts.map(o =>
      `<button onclick="setReplyRestriction('${postId}','${o.value}',this.closest('.rr-modal'))" style="width:100%;padding:10px 16px;text-align:left;background:${o.value===currentDisplay?'var(--accent-dim,rgba(139,92,246,.15))':'none'};border:none;border-radius:8px;color:inherit;cursor:pointer;font-size:14px">${o.label}${o.value===currentDisplay?' ✓':''}</button>`
    ).join('');
    const overlay = document.createElement('div');
    overlay.className = 'rr-modal';
    overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,.6);z-index:9000;display:flex;align-items:center;justify-content:center';
    overlay.innerHTML = `<div style="background:var(--surface-2,#1a1a2e);border-radius:16px;width:min(320px,92vw);padding:16px;display:flex;flex-direction:column;gap:4px">
      <div style="font-weight:600;margin-bottom:8px;font-size:15px">Who can reply?</div>
      ${html}
      <button onclick="this.closest('.rr-modal').remove()" style="margin-top:8px;width:100%;padding:10px;border:1px solid var(--border,#333);border-radius:8px;background:none;color:var(--text-muted,#888);cursor:pointer">Cancel</button>
    </div>`;
    overlay.addEventListener('click', function(e){ if (e.target===overlay) overlay.remove(); });
    document.body.appendChild(overlay);
  }

  async function setReplyRestriction(postId, restriction, modal) {
    const fd = new FormData();
    fd.append('event_type', 'set_reply_restriction');
    fd.append('post_id', postId);
    fd.append('restriction', restriction);
    const r = await fetch('/events', {method:'POST', body:fd});
    if (r.ok || r.status===204) { toast('Reply restriction updated'); modal && modal.remove(); }
    else toast('Update failed', 'error');
  }





  window.goToWork             = goToWork;
  window.toggleWorkReaction   = toggleWorkReaction;
  window.goToPost             = goToPost;
  window.expandFocusBody      = expandFocusBody;
  window.togglePostMenu       = togglePostMenu;
  window.closeAllPostMenus    = closeAllPostMenus;
  window.openEditPost         = openEditPost;
  window.cancelEditPost       = cancelEditPost;
  window.saveEditPost         = saveEditPost;
  window.openReplyRestriction = openReplyRestriction;
  window.setReplyRestriction  = setReplyRestriction;
});
