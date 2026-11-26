# Debian 13 (trixie) for the host smoke test: `make host-test`.
# Only what the host scripts call that the test doesn't stub (test/host/stubs).
FROM debian:trixie
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      nftables iproute2 openssh-server python3 sqlite3 curl ca-certificates sudo \
 && mkdir -p /run/sshd
