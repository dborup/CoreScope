#!/bin/sh
# Offline backup of the two CoreScope SQLite databases, including any WALs.
# Usage: sh scripts/backup-sqlite.sh CONTAINER BACKUP_DIRECTORY
# Requires Docker and ordinary host shell tools; never invokes SQLite or pulls
# an image. The container's /app/data must not have another active writer.
set -eu
umask 077

if [ "$#" -ne 2 ]; then
  echo "usage: $0 CONTAINER BACKUP_DIRECTORY" >&2
  exit 2
fi
container=$1
case "$container" in
  ''|-*|*[!a-zA-Z0-9_.-]*) echo 'invalid container name or ID' >&2; exit 2 ;;
esac

# Preserve an intentionally stopped container's state. Inspection failures are
# fatal before any stop or data copy; no guessing from Docker error text.
running=$(docker inspect --format '{{.State.Running}}' "$container")
case "$running" in true|false) ;; *) echo 'unknown container running state' >&2; exit 1 ;; esac
mkdir -p "$2"
backup_root=$(cd "$2" && pwd -P)
stage=$(mktemp -d "$backup_root/.corescope-backup-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
chmod 700 "$stage"
restart_needed=0

finish() {
  status=$?
  trap - 0 HUP INT TERM
  if [ "$restart_needed" -eq 1 ]; then
    if docker start "$container" >/dev/null; then
      :
    else
      restart_status=$?
      echo "ERROR: restart failed (exit $restart_status); check $container manually" >&2
      if [ "$status" -eq 0 ]; then status=$restart_status; fi
    fi
  fi
  # Only this invocation's fresh, private staging directory is removed.
  case "$stage" in
    "$backup_root"/.corescope-backup-*)
      if ! rm -rf "$stage"; then
        echo "ERROR: could not remove private staging directory $stage" >&2
        if [ "$status" -eq 0 ]; then status=1; fi
      fi ;;
  esac
  exit "$status"
}
trap finish 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if [ "$running" = true ]; then
  # Even a failed stop may have stopped the container before its API call
  # failed. Attempt to restore availability in that case, preserving the error.
  restart_needed=1
  docker stop "$container" >/dev/null
fi
mkdir "$stage/raw" "$stage/snapshot"
# Copy the directory in one operation: absent WALs are naturally absent;
# failing to copy an existing WAL (ENOSPC, permissions, etc.) is a real error.
# The private raw copy can include config/secrets, but only the whitelist below
# is published; enough temporary disk for the complete /app/data is required.
docker cp "$container:/app/data/." "$stage/raw"
for db in meshcore ping_scores_history; do
  file="$stage/raw/$db.db"
  if [ ! -f "$file" ] || [ -L "$file" ] || [ ! -r "$file" ]; then
    echo "ERROR: missing or unreadable regular database $db.db" >&2
    exit 1
  fi
  mv "$file" "$stage/snapshot/$db.db"
  file="$stage/raw/$db.db-wal"
  if [ -e "$file" ] || [ -L "$file" ]; then
    if [ ! -f "$file" ] || [ -L "$file" ] || [ ! -r "$file" ]; then
      echo "ERROR: unreadable regular WAL $db.db-wal" >&2
      exit 1
    fi
    mv "$file" "$stage/snapshot/$db.db-wal"
  fi
done

if [ "$restart_needed" -eq 1 ]; then
  if docker start "$container" >/dev/null; then
    restart_needed=0
  else
    restart_status=$?
    # A returned failure is reported once; a signal during start instead
    # leaves restart_needed set so the EXIT trap attempts recovery.
    restart_needed=0
    echo "ERROR: restart failed (exit $restart_status); backup not published; check $container manually" >&2
    exit "$restart_status"
  fi
fi
# Fresh output directory for every invocation, even several runs in one second.
# Publish only after both databases/WALs were copied and restart succeeded.
name=${stage##*/}
destination="$backup_root/${name#.}"
if [ -e "$destination" ] || [ -L "$destination" ]; then
  echo "ERROR: refusing to overwrite $destination" >&2
  exit 1
fi
mv "$stage/snapshot" "$destination"
printf '%s\n' "$destination"
