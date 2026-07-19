FROM debian:stable-slim
RUN apt-get update && apt-get install -y tor && rm -rf /var/lib/apt/lists/*
COPY torrc /etc/tor/torrc
COPY hs_ed25519_public_key /tmp/hs/hs_ed25519_public_key
COPY hs_ed25519_secret_key /tmp/hs/hs_ed25519_secret_key
COPY hostname              /tmp/hs/hostname
RUN mkdir -p /var/lib/tor/hs \
 && cp /tmp/hs/hs_ed25519_public_key /var/lib/tor/hs/ \
 && cp /tmp/hs/hs_ed25519_secret_key /var/lib/tor/hs/ \
 && cp /tmp/hs/hostname              /var/lib/tor/hs/ \
 && chmod 700 /var/lib/tor /var/lib/tor/hs \
 && chmod 600 /var/lib/tor/hs/hs_ed25519_public_key \
              /var/lib/tor/hs/hs_ed25519_secret_key \
              /var/lib/tor/hs/hostname \
 && chown -R debian-tor:debian-tor /var/lib/tor
USER debian-tor
CMD ["tor", "-f", "/etc/tor/torrc"]
