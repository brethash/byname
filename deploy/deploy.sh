#!/usr/bin/env bash
# shellcheck disable=SC2029 # $STAGE is deliberately expanded locally; it is a path mktemp made on the host.
# deploy.sh — ship the byname binary to a host (Pi or cloud server) and switch to
# it, rolling back automatically if the new version fails its health check.
#
#   deploy/deploy.sh <ssh-target> <version>      (normally via: make deploy TARGET=you@me)
#
# Runs on your Mac. Uses plain ssh, so Tailscale SSH (ssh you@me) works. Picks
# bin/byname-linux-arm64 or bin/byname-linux-amd64 to match the host.
set -euo pipefail

TARGET="${1:?usage: deploy.sh <ssh-target> <version>}"
VERSION="${2:?usage: deploy.sh <ssh-target> <version>}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[[ "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad version string: $VERSION" >&2; exit 1; }
if [[ "$VERSION" == *dirty* && "${FORCE:-0}" != 1 ]]; then
  echo "refusing to deploy a dirty tree ($VERSION); commit first or set FORCE=1" >&2
  exit 1
fi

MACHINE="$(ssh "$TARGET" uname -m)"
case "$MACHINE" in
  aarch64|arm64) ARCH=arm64 ;;
  x86_64|amd64) ARCH=amd64 ;;
  *) echo "unsupported architecture on $TARGET: $MACHINE" >&2; exit 1 ;;
esac
BIN="$ROOT/bin/byname-linux-$ARCH"
[[ -f "$BIN" ]] || { echo "missing $BIN; run: make build-linux" >&2; exit 1; }

SHA="$(shasum -a 256 "$BIN" | cut -d' ' -f1)"
echo "==> deploying byname $VERSION ($ARCH, sha256 $SHA) to $TARGET"

# Stage files in a private temp dir on the host.
STAGE="$(ssh "$TARGET" 'mktemp -d /tmp/byname-deploy.XXXXXX')"
ssh "$TARGET" "cat > '$STAGE/byname'" < "$BIN"
for f in install.sh verify.sh restore.sh; do
  ssh "$TARGET" "cat > '$STAGE/$f'" < "$ROOT/deploy/host/$f"
done
for f in byname.service byname-backup.service byname-backup.timer byname-firewall.service; do
  ssh "$TARGET" "cat > '$STAGE/$f'" < "$ROOT/deploy/systemd/$f"
done

# -t so sudo can prompt for a password if the admin account needs one.
ssh -t "$TARGET" "sudo bash '$STAGE/install.sh' '$STAGE' '$VERSION' '$SHA'; rc=\$?; rm -rf '$STAGE'; exit \$rc"
