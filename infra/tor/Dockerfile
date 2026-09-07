# Tor hidden service for f33d3r.com's .onion.
#
# The image carries NO key material and NO address. The hidden-service
# identity lives in the tor_hs named volume; on first start the entrypoint
# seeds it from a host directory docker-compose mounts read-only at
# /run/secrets/tor (TOR_HS_KEY_DIR on the host) when that directory holds an
# hs_ed25519_secret_key, and otherwise lets tor generate a fresh identity.
# The key is copied rather than used in place because tor insists on owning
# its HiddenServiceDir with mode 0700 and a bind mount carries the host's
# ownership instead. The .onion hostname tor derives from the key is exported
# to the tor_hostname volume (/run/tor/hostname) for feed-engine to read, so
# the address follows the key and is written down nowhere else.
FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends tor && rm -rf /var/lib/apt/lists/*
COPY torrc /etc/tor/torrc
COPY entrypoint.sh /usr/local/bin/tor-entrypoint
RUN chmod 0755 /usr/local/bin/tor-entrypoint \
 && mkdir -p /var/lib/tor \
 && chmod 700 /var/lib/tor \
 && chown debian-tor:debian-tor /var/lib/tor
# Starts as root only to place the key with the ownership tor requires and to
# export the hostname, then drops to debian-tor before tor itself runs.
ENTRYPOINT ["/usr/local/bin/tor-entrypoint"]
