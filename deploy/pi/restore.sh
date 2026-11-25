#!/usr/bin/env bash
# ot-restore — replace the live ot database with a snapshot.
#
#   sudo ot-restore /var/lib/ot/snapshots/ot-20261124T120000.000Z.db
#   sudo ot-restore ~/ot-20261124T120000.000Z.db.gz     (a backup from S3)
#
# Takes a safety snapshot of the current database first, so a restore can
# itself be undone with another ot-restore.
set -euo pipefail

DB=/var/lib/ot/ot.db
SNAPS=/var/lib/ot/snapshots
BIN=/usr/local/lib/ot/current/ot

[[ $EUID -eq 0 ]] || { echo "run as root: sudo ot-restore <snapshot>" >&2; exit 1; }
[[ $# -eq 1 ]] || { echo "usage: sudo ot-restore <snapshot.db | snapshot.db.gz>" >&2; exit 2; }
SRC="$1"
[[ -f "$SRC" ]] || { echo "no such file: $SRC" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
CANDIDATE="$WORK/restore.db"
if [[ "$SRC" == *.gz ]]; then
  gzip -dc "$SRC" > "$CANDIDATE"
else
  cp "$SRC" "$CANDIDATE"
fi

echo "==> checking the snapshot"
result="$(sqlite3 "$CANDIDATE" 'PRAGMA integrity_check;')"
[[ "$result" == "ok" ]] || { echo "integrity check failed: $result" >&2; exit 1; }
links="$(sqlite3 "$CANDIDATE" 'SELECT count(*) FROM links WHERE deleted_at IS NULL;')"
schema="$(sqlite3 "$CANDIDATE" 'PRAGMA user_version;')"
echo "    ok: $links live links, schema version $schema"

if [[ -f "$DB" ]]; then
  echo "==> safety snapshot of the current database"
  sudo -u ot "$BIN" snapshot --db "$DB" --snapshot-dir "$SNAPS" --snapshot-keep 30
fi

echo "==> stopping ot"
systemctl stop ot.service

echo "==> installing the snapshot"
rm -f "$DB-wal" "$DB-shm"
install -o ot -g ot -m 0640 "$CANDIDATE" "$DB"

echo "==> starting ot"
systemctl start ot.service
sleep 2
systemctl is-active --quiet ot.service || { echo "ot failed to start: journalctl -u ot" >&2; exit 1; }
echo "restored $SRC ($links live links)"
