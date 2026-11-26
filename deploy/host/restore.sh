#!/usr/bin/env bash
# byname-restore — replace the live byname database with a snapshot.
#
#   sudo byname-restore --list         the local snapshots, oldest first
#   sudo byname-restore --latest       the newest local snapshot (byname-*.db)
#   sudo byname-restore /var/lib/byname/snapshots/byname-20261124T120000.000Z.db
#   sudo byname-restore ~/byname-20261124T120000.000Z.db.gz     (a backup from S3)
#
# It checks the snapshot, stops byname, keeps the current database (with its
# -wal and -shm files) as snapshots/pre-restore-<time>.db, installs the
# snapshot and starts byname again. Keeping that copy doesn't need the byname
# binary, so a restore also works when the database is damaged, or newer than
# the binary (a rolled-back release that had migrated it). Undo a restore by
# restoring that copy.
#
# Sign-in sessions are cleared in the restored copy, so everyone signs in
# again: a snapshot would otherwise bring back sessions revoked since.
#
# /var/lib/byname belongs to byname, and its snapshots are byname's data: this
# script reads and writes there as byname (or by rename, with byname stopped),
# and opens snapshots in sqlite3's safe mode, so a compromised byname can't
# use a restore to act as root.
set -euo pipefail
umask 027

DB=/var/lib/byname/byname.db
SNAPS=/var/lib/byname/snapshots

die() { echo "byname-restore: $*" >&2; exit 1; }
usage() { echo "usage: sudo byname-restore <snapshot.db | snapshot.db.gz> | --latest | --list" >&2; exit 2; }

[[ $EUID -eq 0 ]] || die "run as root: sudo byname-restore $*"
[[ $# -eq 1 ]] || usage

# A snapshot is data, not code: no view or trigger in it may call writefile()
# and the like with root's rights.
sql() { sqlite3 -safe -cmd 'PRAGMA trusted_schema = OFF;' "$CANDIDATE" "$@"; }
as_byname() { runuser -u byname -- "$@"; }
# Read FILE. Root reads it only when the file it opened is FILE as written:
# outside /var/lib/byname, with no symlink on the way, a regular file. That is
# checked on the open file, so nothing swapped in after the check counts.
# byname's own files, and paths through a symlink, are read with byname's
# rights, so a symlink byname planted can't make root read anything else.
read_file() {
  local plain opened
  plain="$(realpath -s -m -- "$1")" # as written, symlinks not followed
  if [[ $plain != /var/lib/byname/* ]]; then
    {
      opened="$(readlink /proc/self/fd/3)"
      if [[ $opened == "$plain" && -f /proc/self/fd/3 ]]; then
        cat <&3
        return
      fi
    } 3<"$1"
  fi
  as_byname cat -- "$1" ||
    die "can't read $1 as byname (byname's files, and paths through a symlink, are read with byname's rights): copy it to a plain path first"
}

# byname's own snapshots, oldest first (their names sort by time).
snapshots() {
  [[ -d $SNAPS ]] || return 0
  find "$SNAPS" -maxdepth 1 -type f -name 'byname-*.db' -printf '%f\n' | sort
}

case "$1" in
  --list)
    [[ -d $SNAPS ]] || die "no $SNAPS"
    echo "Snapshots in $SNAPS, oldest first (pre-restore-* are copies kept by byname-restore):"
    find "$SNAPS" -maxdepth 1 -type f \( -name 'byname-*.db' -o -name 'pre-restore-*.db' \) \
      -printf '%T@ %TY-%Tm-%Td %TH:%TM  %8s bytes  %p\n' | sort -n | cut -d' ' -f2-
    exit 0
    ;;
  --latest)
    newest="$(snapshots | tail -n 1)"
    [[ -n $newest ]] || die "no snapshots in $SNAPS"
    SRC="$SNAPS/$newest"
    echo "newest snapshot: $SRC"
    ;;
  -*) usage ;;
  *) SRC="$1" ;;
esac
[[ -f "$SRC" ]] || die "no such file: $SRC"

WORK="$(mktemp -d)"
STOPPED=0
cleanup() {
  rm -rf "$WORK"
  if [[ $STOPPED -eq 1 ]]; then
    echo "!! the restore did not finish; starting byname again with the database in place" >&2
    systemctl start byname.service || true
  fi
}
trap cleanup EXIT

CANDIDATE="$WORK/restore.db"
if [[ "$SRC" == *.gz ]]; then
  read_file "$SRC" | gzip -dc > "$CANDIDATE"
else
  read_file "$SRC" > "$CANDIDATE"
  # A pre-restore copy keeps its most recent changes in its -wal file.
  if [[ -f "$SRC-wal" ]]; then read_file "$SRC-wal" > "$CANDIDATE-wal"; fi
fi

echo "==> checking the snapshot"
result="$(sql 'PRAGMA integrity_check;' 2>&1)" || true
[[ "$result" == "ok" ]] || die "$SRC failed its integrity check: $result"
links="$(sql 'SELECT count(*) FROM links WHERE deleted_at IS NULL;' 2>&1)" ||
  die "$SRC is not a usable byname database: $links"
schema="$(sql 'PRAGMA user_version;')"
echo "    ok: $links live links, schema version $schema"
has_sessions="$(sql "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions';")"
if [[ $has_sessions -gt 0 ]]; then
  cleared="$(sql 'SELECT count(*) FROM sessions; DELETE FROM sessions;' 2>&1)" ||
    die "could not clear the sign-in sessions in $SRC: $cleared"
  echo "    cleared $cleared sign-in session(s) in the restored copy: everyone signs in again"
fi
# One self-contained file: fold any -wal into it.
sql 'PRAGMA journal_mode = DELETE;' >/dev/null

echo "==> stopping byname"
STOPPED=1
systemctl stop byname.service

KEPT=""
if [[ -e "$DB" ]]; then
  KEPT="$SNAPS/pre-restore-$(date -u +%Y%m%dT%H%M%S.%3NZ).db"
  echo "==> keeping the current database as $KEPT"
  as_byname mkdir -p "$SNAPS"
  for f in "" -wal -shm; do
    if [[ -e "$DB$f" ]]; then as_byname cp --preserve=timestamps "$DB$f" "$KEPT$f"; fi
  done
fi

echo "==> installing the snapshot"
# Next to the database, then renamed over it: with byname stopped nothing can
# swap these paths, and a rename replaces a planted symlink rather than
# following it.
rm -f "$DB.restoring"
install -o byname -g byname -m 0640 "$CANDIDATE" "$DB.restoring"
rm -f "$DB-wal" "$DB-shm"
mv -fT "$DB.restoring" "$DB"

echo "==> starting byname"
systemctl start byname.service
STOPPED=0
sleep 2
if ! systemctl is-active --quiet byname.service; then
  echo "byname failed to start with the restored database: journalctl -u byname" >&2
  [[ -z $KEPT ]] || echo "to put the previous database back: sudo byname-restore $KEPT" >&2
  exit 1
fi
echo "restored $SRC ($links live links)"
if [[ -n $KEPT ]]; then echo "previous database kept as $KEPT (undo: sudo byname-restore $KEPT)"; fi
