#!/usr/bin/env bash
# install.sh — host side of deploy.sh. Do not run by hand.
#   sudo bash install.sh <stage-dir> <version> <sha256>
#
# 1. verify the binary's checksum
# 2. snapshot the database with the CURRENT binary (before any migration)
# 3. atomically point /usr/local/lib/ot/current at the new version
# 4. restart and health-check; on failure point back and restart
set -euo pipefail

STAGE="$1" VERSION="$2" SHA="$3"
LIB=/usr/local/lib/ot
DB=/var/lib/ot/ot.db
SNAPS=/var/lib/ot/snapshots

[[ $EUID -eq 0 ]] || { echo "must run as root" >&2; exit 1; }
[[ "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad version" >&2; exit 1; }
id ot >/dev/null 2>&1 || { echo "user ot missing; run deploy/host/bootstrap.sh first" >&2; exit 1; }
[[ -f /etc/ot/ot.env ]] || { echo "/etc/ot/ot.env missing; run deploy/host/bootstrap.sh first" >&2; exit 1; }

echo "==> verifying checksum"
echo "$SHA  $STAGE/ot" | sha256sum -c --quiet - || { echo "checksum mismatch; aborting" >&2; exit 1; }

echo "==> installing $LIB/$VERSION"
install -d -m 0755 "$LIB/$VERSION"
install -m 0755 "$STAGE/ot" "$LIB/$VERSION/ot"
"$LIB/$VERSION/ot" version >/dev/null

# Keep helper scripts and units in step with the binary.
install -m 0755 "$STAGE/verify.sh" /usr/local/sbin/ot-verify
install -m 0755 "$STAGE/restore.sh" /usr/local/sbin/ot-restore
units_changed=0
for u in ot.service ot-backup.service ot-backup.timer ot-firewall.service; do
  if ! cmp -s "$STAGE/$u" "/etc/systemd/system/$u"; then
    install -m 0644 "$STAGE/$u" "/etc/systemd/system/$u"
    units_changed=1
  fi
done
[[ $units_changed -eq 1 ]] && systemctl daemon-reload && echo "    updated systemd units"

PREV="$(readlink "$LIB/current" 2>/dev/null || true)"
if [[ -n "$PREV" && -f "$DB" ]]; then
  echo "==> pre-deploy snapshot (with $PREV)"
  sudo -u ot "$LIB/$PREV/ot" snapshot --db "$DB" --snapshot-dir "$SNAPS"
fi

HOST="$(sed -n 's/^OT_HOSTS=\([^,]*\).*/\1/p' /etc/ot/ot.env)"

switch_to() {
  ln -sfn "$1" "$LIB/current.new"
  mv -T "$LIB/current.new" "$LIB/current"
  systemctl restart ot.service
}

healthy() {
  local body
  for _ in $(seq 1 20); do
    # Via localhost (allowed in ot.env), so a deploy doesn't depend on
    # tailscaled being up at that moment. ot-verify tests the tailnet path.
    body="$(curl -fsS -m 3 -H "Host: ${HOST:-ot}" "http://127.0.0.1/_/healthz" 2>/dev/null || true)"
    if grep -q '"status": "ok"' <<<"$body" && grep -q "\"version\": \"$1\"" <<<"$body"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

echo "==> switching to $VERSION"
switch_to "$VERSION"
if healthy "$VERSION"; then
  echo "==> healthy: ot $VERSION is live"
else
  echo "!! $VERSION failed its health check" >&2
  journalctl -u ot.service -n 20 --no-pager >&2 || true
  if [[ -n "$PREV" && "$PREV" != "$VERSION" ]]; then
    echo "==> rolling back to $PREV" >&2
    switch_to "$PREV"
    if healthy "$PREV"; then
      echo "==> rolled back; $PREV is live" >&2
    else
      echo "!! rollback also unhealthy. If $VERSION migrated the schema, restore the" >&2
      echo "   pre-deploy snapshot: sudo ot-restore \$(ls -1 $SNAPS/ot-*.db | tail -1)" >&2
    fi
  fi
  exit 1
fi

echo "==> pruning old versions (keeping current + 2)"
cur="$(readlink "$LIB/current")"
mapfile -t versions < <(find "$LIB" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %f\n' | sort -rn | cut -d' ' -f2-)
kept=0
for v in "${versions[@]}"; do
  [[ "$v" == "$cur" ]] && continue
  kept=$((kept + 1))
  if [[ $kept -gt 2 ]]; then
    rm -rf "${LIB:?}/$v"
    echo "    removed $v"
  fi
done
