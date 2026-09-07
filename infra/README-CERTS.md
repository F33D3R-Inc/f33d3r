# F33D3R Local HTTPS — Device Trust Setup

The local stack runs Caddy as a TLS reverse proxy (`infra/Caddyfile.local`,
`tls internal`). Everything the app serves — pages, `/api/events` SSE, media,
the WHIP publish path and HLS — is behind one listener:

| From | URL |
|---|---|
| This machine | `https://localhost:8443` |
| Any device on the LAN, by Bonjour name | `https://<hostname>.local:8443` (this box: `https://xxxg-00w0.local:8443`) |
| Any device on the LAN, by address | `https://<LIVE_PUBLIC_HOST>:8443` (`LIVE_PUBLIC_HOST` in `.env.local`) |

HTTPS is required for WebCrypto (Gnosis messaging) in Safari and on mobile, and
it is what lets the iOS app talk to this stack with no App Transport Security
exception: ATS accepts a certificate that chains to a root the device trusts,
and a user-installed, fully trusted root counts.

Docker publishes 8443 on every interface and firewalld's `docker-forwarding`
policy lets LAN traffic through, so no firewall change is normally needed. If a
device cannot connect at all (not a certificate error — no answer), open the
port: `sudo firewall-cmd --zone=public --add-port=8443/tcp --permanent && sudo firewall-cmd --reload`.

---

## The certificate file

Caddy generates its own root certificate authority for local development.
Browsers and devices do not trust it until you install it once per device.

The root is committed at `infra/caddy-root.crt` **so that the other machines
can install it from the repo**. It is only useful if it is the root the running
Caddy actually signs with, and Caddy makes a new one whenever its data volume
is reset. Check before trusting:

```bash
# fingerprint of the file in the repo
openssl x509 -in infra/caddy-root.crt -noout -fingerprint -sha256
# fingerprint of the root the running container uses
docker exec f33d3r-caddy-1 cat /data/caddy/pki/authorities/local/root.crt \
  | openssl x509 -noout -fingerprint -sha256
```

If they differ, re-export and commit:

```bash
docker exec f33d3r-caddy-1 cat /data/caddy/pki/authorities/local/root.crt \
  > infra/caddy-root.crt
curl --cacert infra/caddy-root.crt https://localhost:8443/api/health   # must be 200
```

Then reinstall on each device. The leaf certificates rotate every 12 hours on
their own; only the root matters to a device.

---

## Install on macOS

```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  infra/caddy-root.crt
```

Prove it, without `-k`:

```bash
curl https://xxxg-00w0.local:8443/api/health
```

Restart Safari/Chrome afterwards.

---

## Install in the iOS Simulator

The Simulator has its own trust store; the Mac's keychain does not reach it.
With a simulator booted:

```bash
xcrun simctl keychain booted add-root-cert infra/caddy-root.crt
```

Repeat for each simulator device you use (or `xcrun simctl keychain <udid> ...`).

---

## Install on iPhone / iPad (iOS)

1. AirDrop or email `infra/caddy-root.crt` to the device.
2. Tap the file → iOS says "Profile Downloaded".
3. **Settings → General → VPN & Device Management** → tap the profile → **Install**.
4. **Settings → General → About → Certificate Trust Settings** → toggle ON the Caddy root.
5. Open Safari → `https://xxxg-00w0.local:8443` → no warning.

The first time an app on the device reaches a `.local` name or a LAN address,
iOS shows its Local Network permission prompt. Allow it; a denied or missing
permission drops the connection silently (the app sees "could not reach the
server", not a certificate error).

---

## Install on Android (Chrome)

1. Copy `caddy-root.crt` to the device.
2. **Settings → Security → Install a certificate → CA certificate** → select the file.
3. Chrome trusts it after a restart.

The Android app in `mobile/android` does not need this: its debug build talks
plain HTTP to the emulator's host loopback (`http://10.0.2.2:8081`).

---

## mDNS hostname

This machine advertises itself as `<hostname>.local` via avahi (mDNS/Bonjour).
Macs, iPhones and the iOS Simulator resolve it with no configuration. The name
is case-insensitive; the certificate is issued for the lowercase form.

If the hostname or the LAN address changes, update `LIVE_PUBLIC_HOST` in
`.env.local` and the names in `infra/Caddyfile.local` to match, then
`docker compose -f docker-compose.local.yml --env-file .env.local up -d caddy`.
