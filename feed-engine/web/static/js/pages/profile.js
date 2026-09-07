// profile.js — profile page actions. Registered as a Shell page: executed once
// per session, the mount runs on every arrival of a profile Playground.
F33D3R.page('profile', function (root) {
  // ── Online presence indicator ─────────────────────────────────────────────────
  (function() {
    var dot = root.querySelector ? root.querySelector('#profile-online-dot') : document.getElementById('profile-online-dot');
    if (!dot) return;
    var handle = dot.dataset.handle;
    if (!handle) return;
    fetch('/api/online/' + encodeURIComponent(handle))
      .then(function(r) { return r.json(); })
      .then(function(d) {
        if (!dot.isConnected) return;
        dot.style.display = 'block';
        dot.style.background = d.online ? '#22c55e' : '#6b7280';
        dot.title = d.online ? 'Online now' : 'Offline';
      })
      .catch(function() {});
  })();

  let _profileView = 'list';
  function setProfileView(v) {
    _profileView = v;
    const feed = document.getElementById('profile-feed');
    const lb   = document.getElementById('view-list-btn');
    const gb   = document.getElementById('view-grid-btn');
    if (!feed) return;
    if (v === 'grid') {
      feed.classList.add('grid-view');
      if(lb) { lb.style.background='transparent'; lb.style.border='1px solid var(--border)'; lb.style.color='var(--text-muted)'; }
      if(gb) { gb.style.background='color-mix(in srgb,var(--accent) 15%,transparent)'; gb.style.border='1px solid var(--accent)'; gb.style.color='var(--accent)'; }
    } else {
      feed.classList.remove('grid-view');
      if(lb) { lb.style.background='color-mix(in srgb,var(--accent) 15%,transparent)'; lb.style.border='1px solid var(--accent)'; lb.style.color='var(--accent)'; }
      if(gb) { gb.style.background='transparent'; gb.style.border='1px solid var(--border)'; gb.style.color='var(--text-muted)'; }
    }
  }

  // ── Creator subscriptions ────────────────────────────────────────────────────
  async function openSubscribeModal(creatorId, creatorHandle) {
    // Fetch plans
    const r = await fetch('/api/creator/plans?handle=' + encodeURIComponent(creatorHandle));
    const plans = r.ok ? await r.json() : [];

    const modal = document.createElement('div');
    modal.style.cssText = 'position:fixed;inset:0;z-index:200;background:rgba(0,0,0,.6);backdrop-filter:blur(8px);display:flex;align-items:center;justify-content:center';
    modal.onclick = e => { if(e.target===modal) modal.remove(); };

    const plansHtml = plans && plans.length
      ? plans.map(p => `
        <div style="padding:14px;border:1px solid var(--border);border-radius:14px;cursor:pointer;margin-bottom:8px;transition:border-color 120ms"
             onclick="confirmSubscribe('${creatorId}','${p.ID}',${p.PriceAet},this)"
             onmouseover="this.style.borderColor='var(--accent)'" onmouseout="this.style.borderColor='var(--border)'">
          <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px">
            <span style="font-size:14px;font-weight:600;color:var(--text-primary)">${p.Name}</span>
            <span style="font-size:13px;font-weight:600;color:var(--accent)">${(p.PriceAet/100).toFixed(2)} AET/mo</span>
          </div>
          <p style="font-size:12px;color:var(--text-secondary);margin:0">${p.Description}</p>
        </div>`).join('')
      : `<div style="padding:14px;border:1px solid var(--border);border-radius:14px;cursor:pointer;margin-bottom:8px"
              onclick="confirmSubscribe('${creatorId}','',100,this)">
          <div style="display:flex;justify-content:space-between;align-items:center">
            <span style="font-size:14px;font-weight:600;color:var(--text-primary)">Supporter</span>
            <span style="font-size:13px;font-weight:600;color:var(--accent)">1.00 AET/mo</span>
          </div>
          <p style="font-size:12px;color:var(--text-secondary);margin:4px 0 0">Access all exclusive content.</p>
         </div>`;

    modal.innerHTML = `<div style="background:var(--panel);border:1px solid var(--border);border-radius:20px;padding:28px;width:380px;max-width:calc(100vw - 32px)">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:20px">
        <h2 style="font-size:17px;font-weight:600;color:var(--text-primary);font-family:'DM Serif Display',serif">Subscribe to @${creatorHandle}</h2>
        <button onclick="this.closest('[style*=fixed]').remove()" style="width:28px;height:28px;border:none;background:var(--border);border-radius:6px;cursor:pointer;color:var(--text-secondary)">✕</button>
      </div>
      ${plansHtml}
      <p style="font-size:11px;color:var(--text-muted);margin-top:8px;text-align:center">AET is deducted from your wallet. Cancel anytime.</p>
    </div>`;
    document.body.appendChild(modal);
  }

  // A subscription changes what this profile shows (locked content unlocks,
  // the button flips). The server renders that; the Shell asks it for the
  // page again and swaps the fresh Playground in place.
  async function confirmSubscribe(creatorId, planId, priceAet, el) {
    el.style.borderColor = 'var(--accent)';
    el.style.background = 'color-mix(in srgb,var(--accent) 8%,transparent)';
    const fd = new FormData();
    fd.append('creator_id', creatorId);
    if (planId) fd.append('plan_id', planId);
    fd.append('price_aet', priceAet);
    const r = await fetch('/api/subscribe', { method:'POST', body: fd });
    const d = await r.json().catch(() => ({}));
    if (r.ok) {
      document.querySelector('[style*=fixed]:last-child')?.remove();
      toast('Subscribed! Access unlocked.', 'success');
      F33D3R.refresh();
    } else {
      toast(d.message || 'Subscription failed — check your AET balance.', 'error');
      el.style.borderColor = 'var(--border)';
      el.style.background = '';
    }
  }

  async function unsubscribeFrom(creatorId) {
    if (!confirm('Cancel your subscription?')) return;
    const fd = new FormData(); fd.append('creator_id', creatorId);
    const r  = await fetch('/api/unsubscribe', { method:'POST', body: fd });
    if (r.ok) { toast('Subscription cancelled'); F33D3R.refresh(); }
    else toast('Cancel failed', 'error');
  }

  // Facet(notify_btn) — toggle profile notifications
  async function toggleProfileNotify(btn, handle) {
    const active = btn.dataset.active === '1';
    const fd = new FormData();
    fd.append('handle', handle);
    fd.append('enabled', active ? '0' : '1');
    const r = await fetch('/api/notify/profile', { method: 'POST', body: fd });
    if (r.ok) {
      btn.dataset.active = active ? '0' : '1';
      btn.style.color = active ? '' : 'var(--accent)';
      btn.style.borderColor = active ? '' : 'var(--accent)';
      toast(active ? 'Notifications off' : 'You\'ll be notified', 'success');
    } else {
      toast('Could not update notifications', 'error');
    }
  }

  window.setProfileView      = setProfileView;
  window.openSubscribeModal  = openSubscribeModal;
  window.confirmSubscribe    = confirmSubscribe;
  window.unsubscribeFrom     = unsubscribeFrom;
  window.toggleProfileNotify = toggleProfileNotify;
});
