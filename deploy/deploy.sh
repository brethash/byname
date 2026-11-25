#!/usr/bin/env bash
# shellcheck disable=SC2029 # $STAGE is deliberately expanded locally; it is a path mktemp made on the Pi.
# deploy.sh — ship bin/ot-linux-arm64 to the Pi and switch to it, rolling back
# automatically if the new version fails its health check.
#
#   deploy/deploy.sh <ssh-target> <version>      (normally via: make deploy PI=you@ot)
#
# Runs on your Mac. Uses plain ssh, so Tailscale SSH (ssh you@ot) works.
set -euo pipefail

TARGET="${1:?usage: deploy.sh <ssh-target> <version>}"
VERSION="${2:?usage: deploy.sh <ssh-target> <version>}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/ot-linux-arm64"

[[ "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad version string: $VERSION" >&2; exit 1; }
[[ -f "$BIN" ]] || { echo "missing $BIN; run: make build-arm64" >&2; exit 1; }
if [[ "$VERSION" == *dirty* && "${FORCE:-0}" != 1 ]]; then
  echo "refusing to deploy a dirty tree ($VERSION); commit first or set FORCE=1" >&2
  exit 1
fi

SHA="$(shasum -a 256 "$BIN" | cut -d' ' -f1)"
echo "==> deploying ot $VERSION (sha256 $SHA) to $TARGET"

# Stage files in a private temp dir on the Pi.
STAGE="$(ssh "$TARGET" 'mktemp -d /tmp/ot-deploy.XXXXXX')"
ssh "$TARGET" "cat > '$STAGE/ot'" < "$BIN"
ssh "$TARGET" "cat > '$STAGE/install.sh'" < "$ROOT/deploy/pi/install.sh"
for f in verify.sh restore.sh; do
  ssh "$TARGET" "cat > '$STAGE/$f'" < "$ROOT/deploy/pi/$f"
done
for f in ot.service ot-backup.service ot-backup.timer ot-firewall.service; do
  ssh "$TARGET" "cat > '$STAGE/$f'" < "$ROOT/deploy/systemd/$f"
done

# -t so sudo can prompt for a password if the admin account needs one.
ssh -t "$TARGET" "sudo bash '$STAGE/install.sh' '$STAGE' '$VERSION' '$SHA'; rc=\$?; rm -rf '$STAGE'; exit \$rc"
