#!/usr/bin/env bash
# install.sh — host side of deploy.sh. Do not run by hand.
#   sudo bash install.sh <stage-dir> <version> <sha256>
#
# 1. verify the binary's checksum
# 2. snapshot the database with the CURRENT binary (before any migration)
# 3. install the units, and atomically point /usr/local/lib/byname/current at
#    the new version
# 4. restart and health-check; on failure put the previous units and version
#    back and restart
set -euo pipefail

STAGE="$1" VERSION="$2" SHA="$3"
LIB=/usr/local/lib/byname
DB=/var/lib/byname/byname.db
SNAPS=/var/lib/byname/snapshots
UNITS=(byname.service byname-backup.service byname-backup.timer byname-firewall.service)

[[ $EUID -eq 0 ]] || { echo "must run as root" >&2; exit 1; }
[[ "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad version" >&2; exit 1; }
id byname >/dev/null 2>&1 || { echo "user byname missing; run deploy/host/bootstrap.sh first" >&2; exit 1; }
[[ -f /etc/byname/byname.env ]] || { echo "/etc/byname/byname.env missing; run deploy/host/bootstrap.sh first" >&2; exit 1; }

echo "==> verifying checksum"
echo "$SHA  $STAGE/byname" | sha256sum -c --quiet - || { echo "checksum mismatch; aborting" >&2; exit 1; }

echo "==> installing $LIB/$VERSION"
install -d -m 0755 "$LIB/$VERSION"
install -m 0755 "$STAGE/byname" "$LIB/$VERSION/byname"
"$LIB/$VERSION/byname" version >/dev/null

# The helper scripts work with any version, and a failed deploy may need the
# newest byname-restore, so they are updated and never rolled back.
install -m 0755 "$STAGE/verify.sh" /usr/local/sbin/byname-verify
install -m 0755 "$STAGE/restore.sh" /usr/local/sbin/byname-restore

PREV="$(readlink "$LIB/current" 2>/dev/null || true)"
SNAP=""
if [[ -n "$PREV" && -f "$DB" ]]; then
  echo "==> pre-deploy snapshot (with $PREV)"
  SNAP="$(sudo -u byname "$LIB/$PREV/byname" snapshot --db "$DB" --snapshot-dir "$SNAPS" | tail -n 1)"
  echo "    $SNAP"
fi

# Units in step with the binary; the ones replaced are kept for a rollback.
OLD_UNITS="$(mktemp -d)"
trap 'rm -rf "$OLD_UNITS"' EXIT
units_changed=0
for u in "${UNITS[@]}"; do
  if ! cmp -s "$STAGE/$u" "/etc/systemd/system/$u"; then
    if [[ -f /etc/systemd/system/$u ]]; then cp -p "/etc/systemd/system/$u" "$OLD_UNITS/$u"; else touch "$OLD_UNITS/$u.none"; fi
    install -m 0644 "$STAGE/$u" "/etc/systemd/system/$u"
    units_changed=1
  fi
done
if [[ $units_changed -eq 1 ]]; then
  systemctl daemon-reload
  echo "    updated systemd units"
fi

restore_units() {
  local u
  [[ $units_changed -eq 1 ]] || return 0
  for u in "${UNITS[@]}"; do
    if [[ -f $OLD_UNITS/$u ]]; then
      install -m 0644 "$OLD_UNITS/$u" "/etc/systemd/system/$u"
    elif [[ -f $OLD_UNITS/$u.none ]]; then
      rm -f "/etc/systemd/system/$u"
    fi
  done
  systemctl daemon-reload
  echo "    put back the previous systemd units" >&2
}

# The value byname runs with: systemd reads byname.local.env after
# byname.env, so a setting there wins.
env_value() {
  local f line v=""
  for f in /etc/byname/byname.env /etc/byname/byname.local.env; do
    [[ -r $f ]] || continue
    line="$(grep -E "^$1=" "$f" | tail -n 1 || true)"
    if [[ -n $line ]]; then v="${line#*=}"; fi
  done
  v="${v#\"}" v="${v%\"}"
  printf '%s' "$v"
}
# The health check's Host header: the first of the names byname answers to.
HOSTS="$(env_value BYNAME_HOSTS)"
HOST="${HOSTS%%,*}"
HOST="${HOST//[[:space:]]/}"

switch_to() {
  ln -sfn "$1" "$LIB/current.new"
  mv -T "$LIB/current.new" "$LIB/current"
  # A version that crash-looped may have hit systemd's start limit, which
  # would refuse this restart.
  systemctl reset-failed byname.service 2>/dev/null || true
  # A failed restart is the health check's to judge: under set -e it would
  # skip the rollback.
  systemctl restart byname.service || echo "!! systemctl restart byname.service failed" >&2
}

healthy() {
  local body
  for _ in $(seq 1 20); do
    # Via localhost (allowed in byname.env), so a deploy doesn't depend on
    # tailscaled being up at that moment. byname-verify tests the tailnet path.
    body="$(curl -fsS -m 3 -H "Host: ${HOST:-me}" "http://127.0.0.1/_/healthz" 2>/dev/null || true)"
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
  echo "==> healthy: byname $VERSION is live"
else
  echo "!! $VERSION failed its health check" >&2
  journalctl -u byname.service -n 20 --no-pager >&2 || true
  if [[ -n "$PREV" && ( "$PREV" != "$VERSION" || $units_changed -eq 1 ) ]]; then
    echo "==> rolling back to $PREV" >&2
    restore_units
    switch_to "$PREV"
    if healthy "$PREV"; then
      echo "==> rolled back; $PREV is live" >&2
    else
      echo "!! rollback also unhealthy." >&2
      if [[ -n "$SNAP" ]]; then
        echo "   If $VERSION migrated the database, $PREV refuses it. Restore the snapshot" >&2
        echo "   taken just before this deploy:  sudo byname-restore $SNAP" >&2
      fi
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
