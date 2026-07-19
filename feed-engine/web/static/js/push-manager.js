// Facet(push_manager) — F33D3R push notification client
const F33D3RPush = (() => {
  let swRegistration = null;

  async function registerServiceWorker() {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) return null;
    try {
      swRegistration = await navigator.serviceWorker.register('/sw.js', { scope: '/' });
      return swRegistration;
    } catch(err) {
      console.error('[Herald] SW registration failed:', err);
      return null;
    }
  }

  async function checkPermission() {
    if (!('Notification' in window)) return 'unsupported';
    return Notification.permission;
  }

  function urlBase64ToUint8Array(base64String) {
    const padding = '='.repeat((4 - base64String.length % 4) % 4);
    const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    const rawData = window.atob(base64);
    const outputArray = new Uint8Array(rawData.length);
    for (let i = 0; i < rawData.length; ++i) outputArray[i] = rawData.charCodeAt(i);
    return outputArray;
  }

  function getDeviceId() {
    let id = localStorage.getItem('f33d3r_device_id');
    if (!id) { id = crypto.randomUUID(); localStorage.setItem('f33d3r_device_id', id); }
    return id;
  }

  async function subscribe() {
    if (!swRegistration) swRegistration = await registerServiceWorker();
    if (!swRegistration) return { success: false, reason: 'no_sw' };
    try {
      const keyResp = await fetch('/api/push/vapid-key');
      const { public_key } = await keyResp.json();
      const applicationServerKey = urlBase64ToUint8Array(public_key);
      const subscription = await swRegistration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey });
      await fetch('/events', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ event_type: 'push.subscription.registered', subscription: subscription.toJSON(), device_id: getDeviceId() })
      });
      return { success: true };
    } catch(err) {
      if (err.name === 'NotAllowedError') return { success: false, reason: 'denied' };
      return { success: false, reason: 'error', error: err.message };
    }
  }

  async function unsubscribe() {
    if (!swRegistration) return;
    const sub = await swRegistration.pushManager.getSubscription();
    if (sub) {
      await sub.unsubscribe();
      await fetch('/events', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ event_type: 'push.subscription.removed', device_id: getDeviceId() })
      });
    }
  }

  async function init() {
    await registerServiceWorker();
    if (swRegistration) {
      const existingSub = await swRegistration.pushManager.getSubscription();
      if (existingSub) {
        await fetch('/events', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ event_type: 'push.subscription.refreshed', subscription: existingSub.toJSON(), device_id: getDeviceId() })
        });
      }
    }
  }

  return { init, subscribe, unsubscribe, checkPermission };
})();

// PushPrompt — soft permission request UI
const PushPrompt = (() => {
  function show() {
    if (!('Notification' in window)) return;
    if (Notification.permission !== 'default') return;
    const dismissed = localStorage.getItem('f33d3r_push_dismissed');
    if (dismissed && (Date.now() - parseInt(dismissed)) < 7 * 24 * 60 * 60 * 1000) return;
    if (typeof htmx !== 'undefined') {
      htmx.ajax('GET', '/partials/push-prompt', { target: '#portal-modal', swap: 'innerHTML' });
    }
  }

  async function allow() {
    document.getElementById('push-prompt-modal')?.remove();
    localStorage.setItem('f33d3r_push_dismissed', Date.now().toString());

    if (!('Notification' in window)) return;
    const permission = await Notification.requestPermission();
    if (permission === 'granted') {
      localStorage.setItem('f33d3r_push_granted', '1');
      const result = await F33D3RPush.subscribe();
      if (result.success) {
        if (typeof toast === 'function') toast('Notifications enabled', 'success');
      } else {
        console.warn('[Herald] subscribe failed after grant:', result.reason);
      }
    }
  }

  // CHANGE 4: ensure dismiss() always removes the modal
  function dismiss() {
    const modal = document.getElementById('push-prompt-modal');
    if (modal) modal.remove();
    localStorage.setItem('f33d3r_push_dismissed', Date.now().toString());
  }

  return { show, allow, dismiss };
})();

document.addEventListener('DOMContentLoaded', () => {
  // Never run on auth/onboard pages — check for shell-auth or sidebar-hidden class
  const shell = document.querySelector('.f33d3r-shell');
  if (shell && (shell.classList.contains('shell-auth') || shell.classList.contains('auth-shell'))) return;
  F33D3RPush.init();
  // Show the soft prompt whenever permission is still 'default' — not just on first visit.
  // PushPrompt.show() handles the 7-day cooldown for dismissed users internally.
  setTimeout(PushPrompt.show, 5000);
});
