#!/bin/sh
# Hidden-service identity lifecycle.
#
# The .onion address IS the key: tor derives the hostname from
# hs_ed25519_secret_key, so nothing else may claim to know the address. The
# key lives in the tor_hs named volume (HS_DIR). On the very first start:
#
#   1. a key mounted read-only from the host at $SECRET_SRC is seeded into
#      HS_DIR, or
#   2. with no key mounted, tor generates a fresh identity into the empty
#      HiddenServiceDir — never a fatal error.
#
# Once HS_DIR holds a key it is never overwritten: the identity persists in
# the volume across rebuilds and restarts.
#
# When tor has written HS_DIR/hostname, it is exported to $EXPORT_FILE on the
# tor_hostname volume (mounted rw here, read-only in feed-engine at
# /run/tor/hostname) so the application advertises the address the key
# actually produced. The export is done by a background watcher because tor
# writes the file after it starts, and tor only starts once this script execs
# into it.
#
# Runs as root only to satisfy tor's ownership rules (HiddenServiceDir must be
# 0700 and owned by the tor user), then drops to debian-tor.
set -eu

SECRET_DIR="${TOR_HS_SECRET_DIR:-/run/secrets/tor}"
SECRET_SRC="$SECRET_DIR/hs_ed25519_secret_key"
PUBLIC_SRC="$SECRET_DIR/hs_ed25519_public_key"
HS_DIR=/var/lib/tor/hs
EXPORT_DIR="${TOR_HOSTNAME_EXPORT_DIR:-/run/tor}"
EXPORT_FILE="$EXPORT_DIR/hostname"

mkdir -p "$HS_DIR" "$EXPORT_DIR"

if [ -s "$HS_DIR/hs_ed25519_secret_key" ]; then
  echo "tor: using persisted hidden-service identity in $HS_DIR"
elif [ -s "$SECRET_SRC" ]; then
  cp "$SECRET_SRC" "$HS_DIR/hs_ed25519_secret_key"
  # The public half is derived from the secret by tor when absent; seed it
  # when it was shipped alongside so tor does not have to rewrite it.
  if [ -s "$PUBLIC_SRC" ]; then
    cp "$PUBLIC_SRC" "$HS_DIR/hs_ed25519_public_key"
  fi
  echo "tor: seeded hidden-service identity from $SECRET_SRC into $HS_DIR"
else
  echo "tor: no hidden-service key persisted or mounted at $SECRET_SRC — tor will generate a fresh identity in $HS_DIR"
fi

chown -R debian-tor:debian-tor /var/lib/tor
chmod 700 /var/lib/tor "$HS_DIR"
if [ -f "$HS_DIR/hs_ed25519_secret_key" ]; then
  chmod 600 "$HS_DIR/hs_ed25519_secret_key"
fi

# Publish HS_DIR/hostname to the export volume. Written to a sibling temp file
# and renamed so a reader never observes a half-written address. Returns 0
# once the export matches the hostname tor wrote, 1 while tor has not written
# it yet.
export_hostname() {
  if [ ! -s "$HS_DIR/hostname" ]; then
    return 1
  fi
  if [ -s "$EXPORT_FILE" ] && cmp -s "$HS_DIR/hostname" "$EXPORT_FILE"; then
    return 0
  fi
  cp "$HS_DIR/hostname" "$EXPORT_FILE.tmp"
  chmod 644 "$EXPORT_FILE.tmp"
  mv -f "$EXPORT_FILE.tmp" "$EXPORT_FILE"
  echo "tor: hidden-service address $(cat "$EXPORT_FILE") exported to $EXPORT_FILE"
  return 0
}

# A persisted identity already has its hostname: export it before tor starts
# so feed-engine never advertises an address from a previous identity.
export_hostname || true

# Fresh identity (or first export): wait for tor to derive and write the
# hostname, then export it once. Survives the exec below as a child of tor's
# PID and dies with the container.
if [ ! -s "$HS_DIR/hostname" ]; then
  (
    while ! export_hostname; do
      sleep 2
    done
  ) &
fi

exec runuser -u debian-tor -- tor -f /etc/tor/torrc
