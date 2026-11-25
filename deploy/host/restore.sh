#!/usr/bin/env bash
# byname-restore — replace the live byname database with a snapshot.
#
#   sudo byname-restore /var/lib/byname/snapshots/byname-20261124T120000.000Z.db
#   sudo byname-restore ~/byname-20261124T120000.000Z.db.gz     (a backup from S3)
#
# Takes a safety snapshot of the current database first, so a restore can
# itself be undone with another byname-restore.
set -euo pipefail

DB=/var/lib/byname/byname.db
SNAPS=/var/lib/byname/snapshots
BIN=/usr/local/lib/byname/current/byname

[[ $EUID -eq 0 ]] || { echo "run as root: sudo byname-restore <snapshot>" >&2; exit 1; }
[[ $# -eq 1 ]] || { echo "usage: sudo byname-restore <snapshot.db | snapshot.db.gz>" >&2; exit 2; }
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
  sudo -u byname "$BIN" snapshot --db "$DB" --snapshot-dir "$SNAPS" --snapshot-keep 30
fi

echo "==> stopping byname"
systemctl stop byname.service

echo "==> installing the snapshot"
rm -f "$DB-wal" "$DB-shm"
install -o byname -g byname -m 0640 "$CANDIDATE" "$DB"

echo "==> starting byname"
systemctl start byname.service
sleep 2
systemctl is-active --quiet byname.service || { echo "byname failed to start: journalctl -u byname" >&2; exit 1; }
echo "restored $SRC ($links live links)"
