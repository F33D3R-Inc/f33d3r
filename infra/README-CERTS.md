# F33D3R Local HTTPS — Device Trust Setup

This machine runs Caddy as a local TLS reverse proxy.  
HTTPS is required for WebCrypto (messaging encryption) to work in Safari and on mobile.

Server: `https://XXXG-00W0.local:8443`  
Localhost: `https://localhost:8443`

---

## Why you need to install the CA cert

Caddy generates its own root certificate authority (CA) for local development.  
Browsers and Safari/iOS don't trust it by default — you install it once per device.

The cert is at `infra/caddy-root.crt` in this repo.

---

## Install on macOS

```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  infra/caddy-root.crt
```

Then restart Safari/Chrome. Done.

---

## Install on iPhone / iPad (iOS)

1. AirDrop or email `infra/caddy-root.crt` to your iPhone.
2. Tap the file → iOS says "Profile Downloaded".
3. Go to **Settings → General → VPN & Device Management** → tap the profile → **Install**.
4. Go to **Settings → General → About → Certificate Trust Settings** → toggle ON the Caddy cert.
5. Open Safari → go to `https://XXXG-00W0.local:8443` → messaging works.

---

## Install on Android (Chrome)

1. Copy `caddy-root.crt` to the device.
2. **Settings → Security → Install a certificate → CA certificate** → select the file.
3. Chrome will trust it after restart.

---

## Regenerating the cert

If you reset Docker volumes, Caddy generates a new root CA.  
Run this to re-export:

```bash
docker exec f33d3r-local-caddy-1 cat /data/caddy/pki/authorities/local/root.crt \
  > infra/caddy-root.crt
```

Then reinstall on each device.

---

## mDNS hostname

This machine advertises itself as `XXXG-00W0.local` via avahi (mDNS/Bonjour).  
Other devices on the same LAN resolve this automatically — no IP address needed.  
If the machine hostname changes, update `infra/Caddyfile.local` to match.
