// Facet(permissions_manager) — F33D3R media permission handler
const F33D3RPermissions = (() => {
  async function check(permissionName) {
    if (!navigator.permissions) return 'unknown';
    try { const s = await navigator.permissions.query({ name: permissionName }); return s.state; }
    catch(e) { return 'unknown'; }
  }

  async function requestMicrophone() {
    const state = await check('microphone');
    if (state === 'denied') { showDeniedHelp('microphone'); return false; }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });
      stream.getTracks().forEach(t => t.stop());
      return true;
    } catch(err) {
      if (err.name === 'NotAllowedError') showDeniedHelp('microphone');
      else if (err.name === 'NotFoundError') showNoDeviceError('microphone');
      return false;
    }
  }

  async function requestCamera() {
    const state = await check('camera');
    if (state === 'denied') { showDeniedHelp('camera'); return false; }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ video: true, audio: false });
      stream.getTracks().forEach(t => t.stop());
      return true;
    } catch(err) {
      if (err.name === 'NotAllowedError') showDeniedHelp('camera');
      return false;
    }
  }

  function showDeniedHelp(type) {
    const isChrome = navigator.userAgent.includes('Chrome') && !navigator.userAgent.includes('Edge');
    const isFirefox = navigator.userAgent.includes('Firefox');
    let instructions = isChrome
      ? `Click the lock icon in the address bar → Site settings → Allow ${type}`
      : isFirefox
      ? `Click the lock icon → Connection secure → More information → Permissions`
      : `Check your browser settings to allow ${type} for this site`;
    const icon = type === 'microphone' ? '🎤' : '📷';
    const label = type === 'microphone' ? 'Microphone' : 'Camera';
    const modal = document.getElementById('portal-modal');
    if (modal) modal.innerHTML = `<div class="permission-denied-modal"><div class="permission-denied-backdrop" onclick="this.parentElement.innerHTML=''"></div><div class="permission-denied-content"><div class="permission-denied-icon">${icon}</div><h3>${label} access blocked</h3><p>To use this feature, allow access in your browser settings:</p><p class="permission-instructions">${instructions}</p><button class="btn-primary" onclick="this.closest('.permission-denied-modal').remove()">Got it</button></div></div>`;
  }

  function showNoDeviceError(type) {
    const modal = document.getElementById('portal-modal');
    if (modal) modal.innerHTML = `<div class="permission-denied-modal"><div class="permission-denied-content"><p>No ${type} found on this device.</p><button class="btn-primary" onclick="this.closest('.permission-denied-modal').remove()">OK</button></div></div>`;
  }

  return { check, requestMicrophone, requestCamera };
})();
