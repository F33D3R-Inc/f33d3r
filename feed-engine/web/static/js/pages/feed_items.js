const _uuidRe = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function goToPost(id, cid, sid, surface, rank) {
  fetch('/api/feedback', {method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({user_id:'', session_id:sid, surface:surface,
      events:[{content_id:cid, event_type:'click', position_at_display:rank+1,
        timestamp:new Date().toISOString(), exploration_slot:false}]})});
  if (id && _uuidRe.test(id)) window.location = '/post/' + id;
}

function expandFocusBody(id) {
  const el = document.getElementById('fbd-' + id);
  const btn = document.getElementById('fbm-' + id);
  if (!el) return;
  el.classList.toggle('expanded');
  if (btn) btn.textContent = el.classList.contains('expanded') ? 'less' : 'more';
}

async function translatePost(postId, menuWrap) {
  const wrap = document.getElementById('translate-wrap-' + postId);
  if (!wrap) return;
  if (wrap.style.display !== 'none') { wrap.style.display = 'none'; return; }
  const article = menuWrap?.closest('article');
  const bodyEl  = article?.querySelector('.post-body');
  const text    = bodyEl ? bodyEl.innerText.trim() : '';
  if (!text) return;
  wrap.style.display = '';
  wrap.innerHTML = '<span style="color:var(--text-muted)">Translating…</span>';
  try {
    if (window.translation && typeof window.translation.createTranslator === 'function') {
      const t = await window.translation.createTranslator({sourceLanguage:'auto', targetLanguage:'en'});
      wrap.innerHTML = await t.translate(text);
    } else {
      const r = await fetch('https://translate.googleapis.com/translate_a/single?client=gtx&sl=auto&tl=en&dt=t&q=' + encodeURIComponent(text.slice(0,500)));
      const j = await r.json();
      const out = j[0]?.map(s => s[0]).join('') || text;
      const lang = j[2] || 'auto';
      wrap.innerHTML = `<span style="font-size:10px;color:var(--text-muted);display:block;margin-bottom:4px">Translated (${lang} → EN)</span>${out.replace(/</g,'&lt;').replace(/>/g,'&gt;')}`;
    }
  } catch { wrap.innerHTML = '<span style="color:#ef4444;font-size:12px">Translation unavailable.</span>'; }
}

function togglePostMenu(btn) {
  const wrap     = btn.closest('.post-menu-wrap');
  const dropdown = wrap.querySelector('.post-menu-dropdown');
  const isOpen   = dropdown.classList.contains('open');
  closeAllPostMenus();
  if (!isOpen) dropdown.classList.add('open');
}
function closeAllPostMenus() {
  document.querySelectorAll('.post-menu-dropdown.open').forEach(d => d.classList.remove('open'));
}
document.addEventListener('click', function(e) {
  if (!e.target.closest('.post-menu-wrap')) closeAllPostMenus();
});

// Show Edit buttons only for posts within 60-minute window
(function() {
  const EDIT_WINDOW_MS = 60 * 60 * 1000;
  const now = Date.now();
  document.querySelectorAll('.post-edit-btn[data-created]').forEach(btn => {
    const created = parseInt(btn.dataset.created, 10) * 1000;
    if (now - created < EDIT_WINDOW_MS) {
      btn.style.display = '';
      // Auto-hide when window expires
      const remaining = EDIT_WINDOW_MS - (now - created);
      setTimeout(() => { btn.style.display = 'none'; }, remaining);
    }
  });
})();

function openEditPost(postId, btn) {
  const article = btn.closest('article');
  const bodyEl  = article.querySelector('.post-body') || article.querySelector('.focus-body');
  if (!bodyEl) return;
  const currentText = bodyEl.innerText.trim();
  const wrapper = document.createElement('div');
  wrapper.style.cssText = 'padding:8px 0';
  wrapper.innerHTML = `
    <textarea id="edit-ta-${postId}" style="width:100%;min-height:80px;background:var(--panel);border:1px solid var(--accent);border-radius:8px;padding:10px;color:var(--text-primary);font-size:14px;font-family:inherit;resize:vertical;outline:none" maxlength="25000">${currentText.replace(/</g,'&lt;').replace(/>/g,'&gt;')}</textarea>
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
async function saveEditPost(postId) {
  const ta = document.getElementById('edit-ta-' + postId);
  if (!ta) return;
  const body = ta.value.trim();
  if (!body) return;
  const fd = new FormData();
  fd.append('post_id', postId);
  fd.append('body', body);
  try {
    const r = await fetch('/api/post/edit', { method:'POST', body: fd, headers:{'HX-Request':'true'} });
    if (r.ok) {
      toast('Post updated');
      // Refresh the post in the feed
      htmx.trigger(document.body, 'postEdited');
    } else {
      const t = await r.text();
      toast(t || 'Edit failed', 'error');
    }
  } catch(e) { toast('Edit failed: ' + e.message, 'error'); }
}
