function togglePwd() {
  const f = document.getElementById('password-field');
  const ico = document.getElementById('eye-icon');
  if (f.type === 'password') {
    f.type = 'text';
    ico.innerHTML = '<path d="M17.94 17.94A10.07 10.07 0 0112 20c-7 0-11-8-11-8a18.45 18.45 0 015.06-5.94M9.9 4.24A9.12 9.12 0 0112 4c7 0 11 8 11 8a18.5 18.5 0 01-2.16 3.19m-6.72-1.07a3 3 0 11-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/>';
  } else {
    f.type = 'password';
    ico.innerHTML = '<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>';
  }
}

async function signInWithPasskey() {
  if (!navigator.credentials || !window.PublicKeyCredential) {
    alert('Passkeys not supported in this browser. Use handle + password instead.');
    return;
  }
  // Passkey authentication — calls /api/auth/passkey/authenticate
  // Full WebAuthn implementation coming with Thessalon security layer
  alert('Passkey sign-in coming soon. Use your handle to sign in for now.');
}
